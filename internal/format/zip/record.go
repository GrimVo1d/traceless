package zip

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"traceless/internal/format"
)

// Record signatures (APPNOTE 4.3).
const (
	sigLocal        = 0x04034b50
	sigDescriptor   = 0x08074b50
	sigCentral      = 0x02014b50
	sigEnd          = 0x06054b50
	sigEnd64        = 0x06064b50
	sigLocator64    = 0x07064b50
	sigSignature    = 0x05054b50
	sigArchiveExtra = 0x08064b50
)

// Flag bits (APPNOTE 4.4.4).
const (
	flagEncrypted  = 1 << 0
	flagOption1    = 1 << 1 // deflate level hints
	flagOption2    = 1 << 2
	flagDescriptor = 1 << 3  // sizes and CRC follow the data
	flagUTF8       = 1 << 11 // name and comment are UTF-8

	knownFlags = flagEncrypted | flagOption1 | flagOption2 | flagDescriptor | flagUTF8
)

const (
	methodStore   = 0
	methodDeflate = 8
)

// Fixed parts of the records, after the signature.
const (
	localLen   = 26
	centralLen = 42
	endLen     = 18
)

// maxSize is the largest size or offset a record holds without ZIP64:
// 0xffffffff means the real value is in a ZIP64 extra field.
const maxSize = 0xfffffffe

// maxEntries is the most entries an end record counts without ZIP64.
const maxEntries = 0xfffe

// maxNameLen caps entry names. Paths are at most 4096 bytes on Linux and far
// shorter on other systems; longer names are refused.
const maxNameLen = 4096

// Canonical values written by Strip.
const (
	versionMadeBy = 3<<8 | 20 // Unix, ZIP 2.0
	versionNeeded = 20
	dosDate       = 1<<5 | 1 // 1980-01-01, the earliest DOS date
	dosTime       = 0
)

// reader reads the archive in order and counts its offset.
type reader struct {
	r   *bufio.Reader
	off int64
}

func (rd *reader) Read(p []byte) (int, error) {
	n, err := rd.r.Read(p)
	rd.off += int64(n)
	return n, err
}

func (rd *reader) ReadByte() (byte, error) {
	c, err := rd.r.ReadByte()
	if err == nil {
		rd.off++
	}
	return c, err
}

// full fills b; running out of input is malformed.
func (rd *reader) full(b []byte, what string) error {
	if _, err := io.ReadFull(rd, b); err != nil {
		return readErr(err, "truncated "+what)
	}
	return nil
}

// sig reads a record signature.
func (rd *reader) sig() (uint32, error) {
	var b [4]byte
	if err := rd.full(b[:], "archive: no end record"); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}

// limited reads at most n bytes of an entry's data. It is an io.ByteReader,
// so compress/flate reads exactly the bytes of the stream and no further.
type limited struct {
	rd *reader
	n  int64 // bytes left
}

func (l *limited) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, io.EOF
	}
	p = p[:min(int64(len(p)), l.n)]
	n, err := l.rd.Read(p)
	l.n -= int64(n)
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF // the record said there was more
	}
	return n, err
}

func (l *limited) ReadByte() (byte, error) {
	if l.n <= 0 {
		return 0, io.EOF
	}
	c, err := l.rd.ReadByte()
	if err == nil {
		l.n--
	} else if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return c, err
}

// le decodes little-endian fields in order.
type le struct{ b []byte }

func (d *le) u16() uint16 {
	v := binary.LittleEndian.Uint16(d.b)
	d.b = d.b[2:]
	return v
}

func (d *le) u32() uint32 {
	v := binary.LittleEndian.Uint32(d.b)
	d.b = d.b[4:]
	return v
}

// localHeader is a local file header (APPNOTE 4.3.7).
type localHeader struct {
	version, flags, method uint16
	time, date             uint16
	crc, csize, usize      uint32
	name                   string
	extra                  []byte // valid until the next header is read
}

// centralHeader is a central directory record (APPNOTE 4.3.12).
type centralHeader struct {
	madeBy, version, flags, method uint16
	time, date                     uint16
	crc, csize, usize              uint32
	disk, internal                 uint16
	external, offset               uint32
	name                           string
	extra, comment                 []byte   // valid until the next header is read
	tail                           [20]byte // the record's last bytes
}

// endRecord is the end of central directory record (APPNOTE 4.3.16).
type endRecord struct {
	disk, cdDisk, diskEntries, entries uint16
	cdSize, cdOffset                   uint32
	comment                            []byte
	fixed                              [endLen]byte // as read, comment length included
}

// scratchLen fits the variable part of any record: a name, an extra field
// and a comment of up to 65535 bytes each.
const scratchLen = 3 << 16

func (rd *reader) localHeader(scratch []byte) (localHeader, error) {
	var b [localLen]byte
	if err := rd.full(b[:], "local header"); err != nil {
		return localHeader{}, err
	}
	d := le{b[:]}
	h := localHeader{
		version: d.u16(), flags: d.u16(), method: d.u16(),
		time: d.u16(), date: d.u16(),
		crc: d.u32(), csize: d.u32(), usize: d.u32(),
	}
	name, extra, _, err := rd.variable(scratch, d.u16(), d.u16(), 0, "local header")
	h.name, h.extra = name, extra
	return h, err
}

func (rd *reader) centralHeader(scratch []byte) (centralHeader, error) {
	var b [centralLen]byte
	if err := rd.full(b[:], "central directory"); err != nil {
		return centralHeader{}, err
	}
	d := le{b[:]}
	h := centralHeader{
		madeBy: d.u16(), version: d.u16(), flags: d.u16(), method: d.u16(),
		time: d.u16(), date: d.u16(),
		crc: d.u32(), csize: d.u32(), usize: d.u32(),
	}
	nameLen, extraLen, commentLen := d.u16(), d.u16(), d.u16()
	h.disk, h.internal = d.u16(), d.u16()
	h.external, h.offset = d.u32(), d.u32()
	name, extra, comment, err := rd.variable(scratch, nameLen, extraLen, commentLen, "central directory")
	h.name, h.extra, h.comment = name, extra, comment
	// The variable part is contiguous in scratch; the fixed part makes up the
	// rest of the tail when it is short.
	v := scratch[:len(name)+len(extra)+len(comment)]
	k := min(len(h.tail), len(v))
	copy(h.tail[len(h.tail)-k:], v[len(v)-k:])
	copy(h.tail[:len(h.tail)-k], b[len(b)-(len(h.tail)-k):])
	return h, err
}

func (rd *reader) endRecord(scratch []byte) (endRecord, error) {
	var b [endLen]byte
	if err := rd.full(b[:], "end record"); err != nil {
		return endRecord{}, err
	}
	d := le{b[:]}
	e := endRecord{
		disk: d.u16(), cdDisk: d.u16(), diskEntries: d.u16(), entries: d.u16(),
		cdSize: d.u32(), cdOffset: d.u32(), fixed: b,
	}
	_, _, comment, err := rd.variable(scratch, 0, 0, d.u16(), "end record")
	e.comment = comment
	return e, err
}

// variable reads the name, extra field and comment after a record into
// scratch, which holds scratchLen bytes.
func (rd *reader) variable(scratch []byte, nameLen, extraLen, commentLen uint16, what string) (string, []byte, []byte, error) {
	if nameLen > maxNameLen {
		return "", nil, nil, unsupported("entry name of %d bytes", nameLen)
	}
	n, e, c := int(nameLen), int(extraLen), int(commentLen)
	b := scratch[:n+e+c]
	if err := rd.full(b, what); err != nil {
		return "", nil, nil, err
	}
	return string(b[:n]), b[n : n+e], b[n+e:], nil
}

// readDescriptor reads the data descriptor after an entry's data. Its
// signature is optional (APPNOTE 4.3.9.3).
func (rd *reader) readDescriptor() (crc, csize, usize uint32, err error) {
	var b [16]byte
	if err := rd.full(b[:12], "data descriptor"); err != nil {
		return 0, 0, 0, err
	}
	fields := b[:12]
	if binary.LittleEndian.Uint32(b[:]) == sigDescriptor {
		if err := rd.full(b[12:], "data descriptor"); err != nil {
			return 0, 0, 0, err
		}
		fields = b[4:]
	}
	d := le{fields}
	return d.u32(), d.u32(), d.u32(), nil
}

func appendLocal(b []byte, name string, flags, method uint16) []byte {
	b = binary.LittleEndian.AppendUint32(b, sigLocal)
	b = binary.LittleEndian.AppendUint16(b, versionNeeded)
	b = binary.LittleEndian.AppendUint16(b, flags)
	b = binary.LittleEndian.AppendUint16(b, method)
	b = binary.LittleEndian.AppendUint16(b, dosTime)
	b = binary.LittleEndian.AppendUint16(b, dosDate)
	b = append(b, make([]byte, 12)...)                         // CRC and sizes: zero, or in the descriptor
	b = binary.LittleEndian.AppendUint16(b, uint16(len(name))) //nolint:gosec // names are at most maxNameLen
	b = binary.LittleEndian.AppendUint16(b, 0)
	return append(b, name...)
}

func appendDescriptor(b []byte, crc, csize, usize uint32) []byte {
	b = binary.LittleEndian.AppendUint32(b, sigDescriptor)
	b = binary.LittleEndian.AppendUint32(b, crc)
	b = binary.LittleEndian.AppendUint32(b, csize)
	return binary.LittleEndian.AppendUint32(b, usize)
}

func appendCentral(b []byte, e *entry) []byte {
	b = binary.LittleEndian.AppendUint32(b, sigCentral)
	b = binary.LittleEndian.AppendUint16(b, versionMadeBy)
	b = binary.LittleEndian.AppendUint16(b, versionNeeded)
	b = binary.LittleEndian.AppendUint16(b, e.outFlags())
	b = binary.LittleEndian.AppendUint16(b, e.outMethod())
	b = binary.LittleEndian.AppendUint16(b, dosTime)
	b = binary.LittleEndian.AppendUint16(b, dosDate)
	b = binary.LittleEndian.AppendUint32(b, e.out.crc)
	b = binary.LittleEndian.AppendUint32(b, e.out.csize)
	b = binary.LittleEndian.AppendUint32(b, e.out.usize)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(e.name))) //nolint:gosec // names are at most maxNameLen
	b = binary.LittleEndian.AppendUint16(b, 0)                   // extra field
	b = binary.LittleEndian.AppendUint16(b, 0)                   // comment
	b = binary.LittleEndian.AppendUint16(b, 0)                   // disk
	b = binary.LittleEndian.AppendUint16(b, 0)                   // internal attributes
	b = binary.LittleEndian.AppendUint32(b, e.outMode()<<16)
	b = binary.LittleEndian.AppendUint32(b, e.out.offset)
	return append(b, e.name...)
}

func appendEnd(b []byte, entries int, cdSize, cdOffset uint32) []byte {
	b = binary.LittleEndian.AppendUint32(b, sigEnd)
	b = binary.LittleEndian.AppendUint32(b, 0)               // disk numbers
	b = binary.LittleEndian.AppendUint16(b, uint16(entries)) //nolint:gosec // at most maxEntries
	b = binary.LittleEndian.AppendUint16(b, uint16(entries)) //nolint:gosec // at most maxEntries
	b = binary.LittleEndian.AppendUint32(b, cdSize)
	b = binary.LittleEndian.AppendUint32(b, cdOffset)
	return binary.LittleEndian.AppendUint16(b, 0) // comment
}

// readErr reports a short read as a malformed file. Other errors, such as a
// failing disk or a canceled context, are returned as they are.
func readErr(err error, msg string) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return malformed("%s", msg)
	}
	return err
}

func malformed(msg string, args ...any) error {
	return fmt.Errorf("zip: %w: %s", format.ErrMalformed, fmt.Sprintf(msg, args...))
}

func unsupported(msg string, args ...any) error {
	return fmt.Errorf("zip: %w: %s", format.ErrUnsupported, fmt.Sprintf(msg, args...))
}
