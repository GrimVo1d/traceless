package png

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"

	"traceless/internal/format"
)

// signature is the 8-byte header every PNG file starts with.
const signature = "\x89PNG\r\n\x1a\n"

// maxChunkLen is the largest chunk length the PNG spec allows (2^31 - 1).
const maxChunkLen = 1<<31 - 1

type chunkHeader struct {
	Length uint32
	Type   string
}

// critical reports whether the chunk is required for decoding. The case of
// the first letter of the type encodes this: uppercase means critical.
func (h chunkHeader) critical() bool {
	return h.Type[0]&0x20 == 0
}

// chunkReader walks the chunks of a PNG stream. It validates the framing
// (signature, IHDR first, chunk types, lengths, CRCs, IEND last). Only
// ReadAll allocates memory based on a length read from the input, and its
// callers cap that length first.
type chunkReader struct {
	r       io.Reader
	crc     hash.Hash32
	limit   *io.LimitedReader
	data    io.Reader // current chunk data, fed through crc
	open    bool      // current chunk data not yet finished
	pending *chunkHeader
	count   int  // chunks seen
	ended   bool // IEND seen
}

func newChunkReader(r io.Reader) (*chunkReader, error) {
	var sig [len(signature)]byte
	if _, err := io.ReadFull(r, sig[:]); err != nil {
		return nil, readErr(err, "truncated signature")
	}
	if string(sig[:]) != signature {
		return nil, malformed("bad signature")
	}
	return &chunkReader{r: r, crc: crc32.NewIEEE()}, nil
}

// Next finishes the current chunk and reads the next chunk header. It returns
// io.EOF after IEND. Data after IEND is left unread; see Trailing.
func (cr *chunkReader) Next() (chunkHeader, error) {
	if cr.pending != nil {
		h := *cr.pending
		cr.pending = nil
		return h, nil
	}
	if cr.open {
		if err := cr.Finish(); err != nil {
			return chunkHeader{}, err
		}
	}
	if cr.ended {
		return chunkHeader{}, io.EOF
	}

	var buf [8]byte
	if _, err := io.ReadFull(cr.r, buf[:]); err != nil {
		return chunkHeader{}, readErr(err, "truncated chunk header (missing IEND)")
	}
	n := binary.BigEndian.Uint32(buf[:4])
	typ := buf[4:8]
	if n > maxChunkLen {
		return chunkHeader{}, malformed("chunk length %d exceeds limit", n)
	}
	for _, c := range typ {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
			return chunkHeader{}, malformed("invalid chunk type %q", typ)
		}
	}

	h := chunkHeader{Length: n, Type: string(typ)}
	cr.count++
	if cr.count == 1 && h.Type != "IHDR" {
		return chunkHeader{}, malformed("first chunk is %s, not IHDR", h.Type)
	}
	if h.Type == "IEND" {
		if n != 0 {
			return chunkHeader{}, malformed("IEND chunk has data")
		}
		cr.ended = true
	}

	cr.crc.Reset()
	cr.crc.Write(typ)
	cr.limit = &io.LimitedReader{R: cr.r, N: int64(n)}
	cr.data = io.TeeReader(cr.limit, cr.crc)
	cr.open = true
	return h, nil
}

// Unread makes the next call to Next return h again. h must be the header of
// the current, still unread chunk.
func (cr *chunkReader) Unread(h chunkHeader) {
	cr.pending = &h
}

// Data returns a reader over the current chunk's data.
func (cr *chunkReader) Data() io.Reader {
	return cr.data
}

// ReadAll reads the whole current chunk and checks its CRC. Callers must check
// the length first: it allocates h.Length bytes.
func (cr *chunkReader) ReadAll(h chunkHeader) ([]byte, error) {
	data := make([]byte, h.Length)
	if _, err := io.ReadFull(cr.data, data); err != nil {
		return nil, readErr(err, "truncated chunk data")
	}
	if err := cr.Finish(); err != nil {
		return nil, err
	}
	return data, nil
}

// Finish skips any unread data of the current chunk and checks its CRC.
func (cr *chunkReader) Finish() error {
	var stored [4]byte
	cr.open = false
	if _, err := io.Copy(io.Discard, cr.data); err != nil {
		return err
	}
	if cr.limit.N != 0 {
		return malformed("truncated chunk data")
	}
	if _, err := io.ReadFull(cr.r, stored[:]); err != nil {
		return readErr(err, "truncated chunk CRC")
	}
	if binary.BigEndian.Uint32(stored[:]) != cr.crc.Sum32() {
		return malformed("chunk CRC mismatch")
	}
	return nil
}

// Trailing counts the bytes after IEND. Call it only after Next returned io.EOF.
func (cr *chunkReader) Trailing() (int64, error) {
	return io.Copy(io.Discard, cr.r)
}

// writeChunk writes one complete chunk with a freshly computed CRC.
func writeChunk(w io.Writer, typ string, data []byte) error {
	var buf [8]byte
	binary.BigEndian.PutUint32(buf[:4], uint32(len(data)))
	copy(buf[4:], typ)
	if _, err := w.Write(buf[:]); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	crc := crc32.NewIEEE()
	crc.Write(buf[4:])
	crc.Write(data)
	binary.BigEndian.PutUint32(buf[:4], crc.Sum32())
	_, err := w.Write(buf[:4])
	return err
}

// chunkWriter splits everything written to it into chunks of one type and a
// fixed maximum size.
type chunkWriter struct {
	w   io.Writer
	typ string
	buf []byte
}

func newChunkWriter(w io.Writer, typ string, size int) *chunkWriter {
	return &chunkWriter{w: w, typ: typ, buf: make([]byte, 0, size)}
}

func (cw *chunkWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		k := copy(cw.buf[len(cw.buf):cap(cw.buf)], p)
		cw.buf = cw.buf[:len(cw.buf)+k]
		p = p[k:]
		if len(cw.buf) == cap(cw.buf) {
			if err := cw.Flush(); err != nil {
				return 0, err
			}
		}
	}
	return n, nil
}

// Flush writes any buffered data as a final, shorter chunk.
func (cw *chunkWriter) Flush() error {
	if len(cw.buf) == 0 {
		return nil
	}
	err := writeChunk(cw.w, cw.typ, cw.buf)
	cw.buf = cw.buf[:0]
	return err
}

// readErr reports a short read as a malformed file. Other errors, such as a
// failing disk or a cancelled context, are returned as they are.
func readErr(err error, msg string) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return malformed("%s", msg)
	}
	return err
}

func malformed(msg string, args ...any) error {
	return fmt.Errorf("png: %w: %s", format.ErrMalformed, fmt.Sprintf(msg, args...))
}

func unsupported(msg string, args ...any) error {
	return fmt.Errorf("png: %w: %s", format.ErrUnsupported, fmt.Sprintf(msg, args...))
}
