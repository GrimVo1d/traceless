package jpeg

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"traceless/internal/format"
)

// Markers. Every segment starts with 0xFF and one of these bytes.
const (
	markerSOF0  = 0xc0 // baseline
	markerSOF1  = 0xc1 // extended sequential, Huffman coding
	markerSOF2  = 0xc2 // progressive
	markerDHT   = 0xc4
	markerSOF15 = 0xcf
	markerDAC   = 0xcc // arithmetic coding conditioning
	markerRST0  = 0xd0
	markerRST7  = 0xd7
	markerSOI   = 0xd8
	markerEOI   = 0xd9
	markerSOS   = 0xda
	markerDQT   = 0xdb
	markerDNL   = 0xdc
	markerDRI   = 0xdd
	markerAPP0  = 0xe0
	markerAPP15 = 0xef
	markerCOM   = 0xfe
)

// markerName returns a readable name for error messages and fields.
func markerName(m byte) string {
	switch {
	case m >= markerAPP0 && m <= markerAPP15:
		return fmt.Sprintf("APP%d", m-markerAPP0)
	case m >= markerRST0 && m <= markerRST7:
		return fmt.Sprintf("RST%d", m-markerRST0)
	case m >= markerSOF0 && m <= markerSOF15 && m != markerDHT && m != 0xc8 && m != markerDAC:
		return fmt.Sprintf("SOF%d", m-markerSOF0)
	}
	switch m {
	case markerDHT:
		return "DHT"
	case markerDAC:
		return "DAC"
	case markerSOI:
		return "SOI"
	case markerEOI:
		return "EOI"
	case markerSOS:
		return "SOS"
	case markerDQT:
		return "DQT"
	case markerDNL:
		return "DNL"
	case markerDRI:
		return "DRI"
	case markerCOM:
		return "COM"
	}
	return fmt.Sprintf("marker 0x%02X", m)
}

// markerReader walks the segments of a JPEG stream. Every segment length is
// at most 65535 by format, so reading a whole segment is bounded.
type markerReader struct {
	r       *bufio.Reader
	pending byte  // marker found by the scan reader, returned by next
	stray   int64 // bytes before markers that decoders skip
}

func newMarkerReader(r io.Reader) (*markerReader, error) {
	br := bufio.NewReader(r)
	var soi [2]byte
	if _, err := io.ReadFull(br, soi[:]); err != nil {
		return nil, readErr(err, "truncated SOI")
	}
	if soi != [2]byte{0xff, markerSOI} {
		return nil, malformed("missing SOI")
	}
	return &markerReader{r: br}, nil
}

// next returns the next marker. Decoders skip bytes that don't form a marker
// and any number of 0xFF fill bytes before one; those are counted in stray,
// since they can carry data.
func (mr *markerReader) next() (byte, error) {
	if m := mr.pending; m != 0 {
		mr.pending = 0
		return m, nil
	}
	var skipped int64
	for {
		b, err := mr.r.ReadByte()
		if err != nil {
			return 0, readErr(err, "truncated file (missing EOI)")
		}
		if b != 0xff {
			skipped++
			continue
		}
		for b == 0xff {
			if b, err = mr.r.ReadByte(); err != nil {
				return 0, readErr(err, "truncated file (missing EOI)")
			}
			skipped++
		}
		skipped-- // the last 0xFF belongs to the marker
		if b == 0 {
			skipped += 2 // 0xFF 0x00 is not a marker
			continue
		}
		mr.stray += skipped
		return b, nil
	}
}

// segment reads the data of a marker segment: a two-byte length that counts
// itself, then the data.
func (mr *markerReader) segment(m byte) ([]byte, error) {
	var buf [2]byte
	if _, err := io.ReadFull(mr.r, buf[:]); err != nil {
		return nil, readErr(err, "truncated "+markerName(m)+" segment")
	}
	n := int(binary.BigEndian.Uint16(buf[:]))
	if n < 2 {
		return nil, malformed("%s segment length %d", markerName(m), n)
	}
	data := make([]byte, n-2)
	if _, err := io.ReadFull(mr.r, data); err != nil {
		return nil, readErr(err, "truncated "+markerName(m)+" segment")
	}
	return data, nil
}

// trailing counts the bytes after EOI and reports whether they start like
// another JPEG file, which phones append for depth maps and motion photos.
func (mr *markerReader) trailing() (n int64, image bool, err error) {
	head, _ := mr.r.Peek(3)
	image = string(head) == "\xff\xd8\xff"
	n, err = io.Copy(io.Discard, mr.r)
	return n, image, err
}

// appendSegment appends a marker segment with its length.
func appendSegment(b []byte, m byte, data []byte) []byte {
	b = append(b, 0xff, m)
	b = binary.BigEndian.AppendUint16(b, uint16(len(data)+2)) //nolint:gosec // segments are built from bounded tables
	return append(b, data...)
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
	return fmt.Errorf("jpeg: %w: %s", format.ErrMalformed, fmt.Sprintf(msg, args...))
}

func unsupported(msg string, args ...any) error {
	return fmt.Errorf("jpeg: %w: %s", format.ErrUnsupported, fmt.Sprintf(msg, args...))
}
