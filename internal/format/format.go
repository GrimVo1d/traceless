// Package format defines the interface every format handler implements and
// the detection logic that picks a handler for an input.
package format

import (
	"bufio"
	"errors"
	"io"
)

var (
	// ErrUnknownFormat means no handler recognized the input. Unknown files
	// are never passed through as clean.
	ErrUnknownFormat = errors.New("unknown or unsupported file format")

	// ErrMalformed means the input claims to be a known format but violates it.
	ErrMalformed = errors.New("malformed file")

	// ErrUnsupported means the input is valid but uses a feature the handler
	// cannot process safely.
	ErrUnsupported = errors.New("unsupported file feature")

	// ErrMetadataRemains means verification found metadata in a stripped file.
	ErrMetadataRemains = errors.New("metadata remains")
)

// Field is one piece of metadata found in a file.
type Field struct {
	Name  string
	Value string
}

// Handler inspects and strips metadata for one container format.
type Handler interface {
	// Name returns a short format name, e.g. "PNG".
	Name() string

	// Match reports whether header, the first SniffLen bytes of the input
	// (fewer for short inputs), belongs to this format.
	Match(header []byte) bool

	// Inspect lists the metadata found in r.
	Inspect(r io.Reader) ([]Field, error)

	// Strip copies r to w without metadata. On error, w may hold partial
	// output and must be discarded.
	Strip(r io.Reader, w io.Writer) error

	// Verify returns an error wrapping ErrMetadataRemains if r still holds
	// metadata, or another error if r is not a valid file of this format.
	Verify(r io.Reader) error
}

// SniffLen is the number of leading bytes handlers get for detection.
const SniffLen = 512

// Detect peeks at br and returns the first handler that matches. It does not
// consume input, so br can be passed on to the handler.
func Detect(br *bufio.Reader, handlers []Handler) (Handler, error) {
	header, err := br.Peek(SniffLen)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	for _, h := range handlers {
		if h.Match(header) {
			return h, nil
		}
	}
	return nil, ErrUnknownFormat
}
