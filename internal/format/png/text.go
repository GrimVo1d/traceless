package png

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"strings"

	"traceless/internal/format"
	"traceless/internal/format/exif"
)

const (
	// maxChunkRead caps how much of one metadata chunk Inspect reads into memory.
	maxChunkRead = 1 << 20

	// maxValueLen caps the length of a reported value, including decompressed
	// text, so a small zTXt chunk can't expand into gigabytes.
	maxValueLen = 4096

	// maxNameLen caps a reported name. Keywords are at most 79 bytes by spec,
	// but nothing stops a file from using longer ones.
	maxNameLen = 128
)

// textDecoder decodes the chunks Inspect shows as text, spending from
// budget. It reuses one zlib reader for all of them.
type textDecoder struct {
	budget *format.Budget
	zr     io.ReadCloser
}

// describe turns a dropped chunk into a human-readable field. Generic chunks
// carry no readable value and can be grouped by type.
func (td *textDecoder) describe(h chunkHeader, data io.Reader) (f format.Field, grouped bool, err error) {
	switch h.Type {
	case "tEXt", "zTXt", "iTXt", "tIME", "iCCP":
	default:
		return format.Field{Name: h.Type + " chunk", Value: fmt.Sprintf("%d bytes", h.Length)}, true, nil
	}

	n := min(int64(h.Length), maxChunkRead)
	if err := td.budget.Hold(n); err != nil {
		return format.Field{}, false, err
	}
	defer td.budget.Release(n)
	if h.Type == "zTXt" || h.Type == "iTXt" {
		// What inflate may decompress, charged before it runs.
		if err := td.budget.Inflate(maxValueLen + 1); err != nil {
			return format.Field{}, false, err
		}
	}
	// One buffer of the size held: ReadAll's growth would need about twice.
	b := make([]byte, n)
	k, err := io.ReadFull(data, b)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return format.Field{}, false, err
	}
	b = b[:k]

	var name, value string
	var ok bool
	switch h.Type {
	case "tEXt":
		name, value, ok = parseText(b)
	case "zTXt":
		name, value, ok = td.parseZText(b)
	case "iTXt":
		name, value, ok = td.parseIText(b)
	case "tIME":
		name, value, ok = parseTime(b)
	case "iCCP":
		name, value, ok = parseICCP(b, h.Length)
	}
	if !ok {
		return format.Field{Name: h.Type + " chunk", Value: "(malformed)"}, false, nil
	}
	if h.Length > maxChunkRead && h.Type != "iCCP" {
		value += " (truncated)"
	}
	return format.Field{Name: clip(name, maxNameLen), Value: clip(value, maxValueLen)}, false, nil
}

// describeEXIF decodes an eXIf chunk into one field per tag. EXIF that can't
// be fully decoded is still reported, with what was read and the reason. data
// is nil if the chunk is longer than maxChunkRead.
func describeEXIF(h chunkHeader, data []byte) []format.Field {
	if h.Length > maxChunkRead {
		return []format.Field{{Name: "EXIF", Value: fmt.Sprintf("%d bytes (too large to decode)", h.Length)}}
	}
	fields, err := exif.Parse(data)
	switch {
	case err != nil:
		fields = append(fields, format.Field{Name: "EXIF", Value: fmt.Sprintf("%d bytes, %v", h.Length, err)})
	case len(fields) == 0:
		fields = append(fields, format.Field{Name: "EXIF", Value: fmt.Sprintf("%d bytes, no tags", h.Length)})
	}
	return fields
}

// parseText decodes a tEXt chunk: keyword, NUL, Latin-1 text.
func parseText(b []byte) (name, value string, ok bool) {
	key, text, ok := bytes.Cut(b, []byte{0})
	if !ok {
		return "", "", false
	}
	return "tEXt " + latin1(key), latin1(text), true
}

// parseZText decodes a zTXt chunk: keyword, NUL, method, zlib Latin-1 text.
func (td *textDecoder) parseZText(b []byte) (name, value string, ok bool) {
	key, rest, ok := bytes.Cut(b, []byte{0})
	if !ok || len(rest) < 1 || rest[0] != 0 {
		return "", "", false
	}
	text, err := td.inflate(rest[1:])
	if err != nil {
		return "", "", false
	}
	return "zTXt " + latin1(key), latin1(text), true
}

// parseIText decodes an iTXt chunk: keyword, NUL, compression flag, method,
// language tag, NUL, translated keyword, NUL, UTF-8 text (zlib if flagged).
func (td *textDecoder) parseIText(b []byte) (name, value string, ok bool) {
	key, rest, ok := bytes.Cut(b, []byte{0})
	if !ok || len(rest) < 2 {
		return "", "", false
	}
	compressed, method := rest[0], rest[1]
	lang, rest, ok := bytes.Cut(rest[2:], []byte{0})
	if !ok {
		return "", "", false
	}
	_, text, ok := bytes.Cut(rest, []byte{0})
	if !ok {
		return "", "", false
	}
	if compressed == 1 {
		if method != 0 {
			return "", "", false
		}
		var err error
		if text, err = td.inflate(text); err != nil {
			return "", "", false
		}
	}
	name = "iTXt " + latin1(key)
	if len(lang) > 0 {
		name += " [" + latin1(lang) + "]"
	}
	return name, string(text[:min(len(text), maxValueLen+1)]), true
}

// parseTime decodes a tIME chunk: the last modification time.
func parseTime(b []byte) (name, value string, ok bool) {
	if len(b) != 7 {
		return "", "", false
	}
	year := int(b[0])<<8 | int(b[1])
	return "Modified", fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d UTC",
		year, b[2], b[3], b[4], b[5], b[6]), true
}

// parseICCP reports the name of an embedded ICC profile.
func parseICCP(b []byte, size uint32) (name, value string, ok bool) {
	profile, _, ok := bytes.Cut(b, []byte{0})
	if !ok {
		return "", "", false
	}
	return "ICC profile", fmt.Sprintf("%s (%d bytes)", latin1(profile), size), true
}

// inflate decompresses zlib data, stopping just past maxValueLen bytes.
func (td *textDecoder) inflate(b []byte) ([]byte, error) {
	if td.zr == nil {
		zr, err := zlib.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		td.zr = zr
	} else if err := td.zr.(zlib.Resetter).Reset(bytes.NewReader(b), nil); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(td.zr, maxValueLen+1))
}

// latin1 converts ISO 8859-1 bytes to a UTF-8 string. It converts at most
// maxValueLen + 1 bytes: clip cuts anything longer anyway.
func latin1(b []byte) string {
	b = b[:min(len(b), maxValueLen+1)]
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		sb.WriteRune(rune(c))
	}
	return sb.String()
}

// clip shortens s to at most n bytes and replaces invalid UTF-8.
func clip(s string, n int) string {
	if len(s) > n {
		s = s[:n] + "… (truncated)"
	}
	return strings.ToValidUTF8(s, "\xef\xbf\xbd")
}
