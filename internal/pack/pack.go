package pack

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/restic"

	"github.com/restic/restic/internal/crypto"
)

// Packer is used to create a new Pack.
type Packer struct {
	blobs []restic.Blob

	bytes uint
	k     *crypto.Key
	wr    io.Writer

	m sync.Mutex
}

// NewPacker returns a new Packer that can be used to pack blobs
// together. If wr is nil, a bytes.Buffer is used.
func NewPacker(k *crypto.Key, wr io.Writer) *Packer {
	if wr == nil {
		wr = bytes.NewBuffer(nil)
	}
	return &Packer{k: k, wr: wr}
}

// Add saves the data read from rd as a new blob to the packer. Returned is the
// number of bytes written to the pack.
func (p *Packer) Add(t restic.BlobType, id restic.ID, data []byte) (int, error) {
	p.m.Lock()
	defer p.m.Unlock()

	c := restic.Blob{Type: t, ID: id}

	n, err := p.wr.Write(data)
	c.Length = uint(n)
	c.Offset = p.bytes
	p.bytes += uint(n)
	p.blobs = append(p.blobs, c)

	return n, errors.Wrap(err, "Write")
}

var entrySize = uint(binary.Size(restic.BlobType(0)) + binary.Size(uint32(0)) + len(restic.ID{}))

// headerEntry is used with encoding/binary to read and write header entries
type headerEntry struct {
	Type   uint8
	Length uint32
	ID     restic.ID
}

// Finalize writes the header for all added blobs and finalizes the pack.
// Returned are the number of bytes written, including the header. If the
// underlying writer implements io.Closer, it is closed.
func (p *Packer) Finalize() (uint, error) {
	p.m.Lock()
	defer p.m.Unlock()

	bytesWritten := p.bytes

	hdrBuf := bytes.NewBuffer(nil)
	bytesHeader, err := p.writeHeader(hdrBuf)
	if err != nil {
		return 0, err
	}

	encryptedHeader, err := p.k.Encrypt(nil, hdrBuf.Bytes())
	if err != nil {
		return 0, err
	}

	// append the header
	n, err := p.wr.Write(encryptedHeader)
	if err != nil {
		return 0, errors.Wrap(err, "Write")
	}

	hdrBytes := restic.CiphertextLength(int(bytesHeader))
	if n != hdrBytes {
		return 0, errors.New("wrong number of bytes written")
	}

	bytesWritten += uint(hdrBytes)

	// write length
	err = binary.Write(p.wr, binary.LittleEndian, uint32(restic.CiphertextLength(len(p.blobs)*int(entrySize))))
	if err != nil {
		return 0, errors.Wrap(err, "binary.Write")
	}
	bytesWritten += uint(binary.Size(uint32(0)))

	p.bytes = uint(bytesWritten)

	if w, ok := p.wr.(io.Closer); ok {
		return bytesWritten, w.Close()
	}

	return bytesWritten, nil
}

// writeHeader constructs and writes the header to wr.
func (p *Packer) writeHeader(wr io.Writer) (bytesWritten uint, err error) {
	for _, b := range p.blobs {
		entry := headerEntry{
			Length: uint32(b.Length),
			ID:     b.ID,
		}

		switch b.Type {
		case restic.DataBlob:
			entry.Type = 0
		case restic.TreeBlob:
			entry.Type = 1
		default:
			return 0, errors.Errorf("invalid blob type %v", b.Type)
		}

		err := binary.Write(wr, binary.LittleEndian, entry)
		if err != nil {
			return bytesWritten, errors.Wrap(err, "binary.Write")
		}

		bytesWritten += entrySize
	}

	return
}

// Size returns the number of bytes written so far.
func (p *Packer) Size() uint {
	p.m.Lock()
	defer p.m.Unlock()

	return p.bytes
}

// Count returns the number of blobs in this packer.
func (p *Packer) Count() int {
	p.m.Lock()
	defer p.m.Unlock()

	return len(p.blobs)
}

// Blobs returns the slice of blobs that have been written.
func (p *Packer) Blobs() []restic.Blob {
	p.m.Lock()
	defer p.m.Unlock()

	return p.blobs
}

// Writer return the underlying writer.
func (p *Packer) Writer() io.Writer {
	return p.wr
}

func (p *Packer) String() string {
	return fmt.Sprintf("<Packer %d blobs, %d bytes>", len(p.blobs), p.bytes)
}

// readHeaderLength returns the header length read from the end of the file
// encoded in little endian.
func readHeaderLength(rd io.ReaderAt, size int64) (uint32, error) {
	off := size - int64(binary.Size(uint32(0)))

	buf := make([]byte, binary.Size(uint32(0)))
	n, err := rd.ReadAt(buf, off)
	if err != nil {
		return 0, errors.Wrap(err, "ReadAt")
	}

	if n != len(buf) {
		return 0, errors.New("not enough bytes read")
	}

	return binary.LittleEndian.Uint32(buf), nil
}

const maxHeaderSize = 16 * 1024 * 1024

// we require at least one entry in the header, and one blob for a pack file
var minFileSize = entrySize + crypto.Extension

// readHeader reads the header at the end of rd. size is the length of the
// whole data accessible in rd.
func readHeader(rd io.ReaderAt, size int64) ([]byte, error) {
	debug.Log("size: %v", size)
	if size == 0 {
		err := InvalidFileError{Message: "file is empty"}
		return nil, errors.Wrap(err, "readHeader")
	}

	if size < int64(minFileSize) {
		err := InvalidFileError{Message: "file is too small"}
		return nil, errors.Wrap(err, "readHeader")
	}

	hl, err := readHeaderLength(rd, size)
	if err != nil {
		return nil, err
	}

	debug.Log("header length: %v", size)

	if hl == 0 {
		err := InvalidFileError{Message: "header length is zero"}
		return nil, errors.Wrap(err, "readHeader")
	}

	if hl < crypto.Extension {
		err := InvalidFileError{Message: "header length is too small"}
		return nil, errors.Wrap(err, "readHeader")
	}

	if (hl-crypto.Extension)%uint32(entrySize) != 0 {
		err := InvalidFileError{Message: "header length is invalid"}
		return nil, errors.Wrap(err, "readHeader")
	}

	if int64(hl) > size-int64(binary.Size(hl)) {
		err := InvalidFileError{Message: "header is larger than file"}
		return nil, errors.Wrap(err, "readHeader")
	}

	if int64(hl) > maxHeaderSize {
		err := InvalidFileError{Message: "header is larger than maxHeaderSize"}
		return nil, errors.Wrap(err, "readHeader")
	}

	buf := make([]byte, int(hl))
	n, err := rd.ReadAt(buf, size-int64(hl)-int64(binary.Size(hl)))
	if err != nil {
		return nil, errors.Wrap(err, "ReadAt")
	}

	if n != len(buf) {
		return nil, errors.New("not enough bytes read")
	}

	return buf, nil
}

// InvalidFileError is return when a file is found that is not a pack file.
type InvalidFileError struct {
	Message string
}

func (e InvalidFileError) Error() string {
	return e.Message
}

// List returns the list of entries found in a pack file.
func List(k *crypto.Key, rd io.ReaderAt, size int64) (entries []restic.Blob, err error) {
	buf, err := readHeader(rd, size)
	if err != nil {
		return nil, err
	}

	n, err := k.Decrypt(buf, buf)
	if err != nil {
		return nil, err
	}
	buf = buf[:n]

	hdrRd := bytes.NewReader(buf)

	entries = make([]restic.Blob, 0, uint(n)/entrySize)

	pos := uint(0)
	for {
		e := headerEntry{}
		err = binary.Read(hdrRd, binary.LittleEndian, &e)
		if errors.Cause(err) == io.EOF {
			break
		}

		if err != nil {
			return nil, errors.Wrap(err, "binary.Read")
		}

		entry := restic.Blob{
			Length: uint(e.Length),
			ID:     e.ID,
			Offset: pos,
		}

		switch e.Type {
		case 0:
			entry.Type = restic.DataBlob
		case 1:
			entry.Type = restic.TreeBlob
		default:
			return nil, errors.Errorf("invalid type %d", e.Type)
		}

		entries = append(entries, entry)

		pos += uint(e.Length)
	}

	return entries, nil
}
