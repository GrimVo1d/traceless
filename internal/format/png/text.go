package png

import (
	"bytes"
	"compress/zlib"
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

// describe turns a dropped chunk into a human-readable field. Generic chunks
// carry no readable value and can be grouped by type.
func describe(h chunkHeader, data io.Reader) (f format.Field, grouped bool, err error) {
	switch h.Type {
	case "tEXt", "zTXt", "iTXt", "tIME", "iCCP":
	default:
		return format.Field{Name: h.Type + " chunk", Value: fmt.Sprintf("%d bytes", h.Length)}, true, nil
	}

	b, err := io.ReadAll(io.LimitReader(data, maxChunkRead))
	if err != nil {
		return format.Field{}, false, err
	}

	var name, value string
	var ok bool
	switch h.Type {
	case "tEXt":
		name, value, ok = parseText(b)
	case "zTXt":
		name, value, ok = parseZText(b)
	case "iTXt":
		name, value, ok = parseIText(b)
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
// be fully decoded is still reported, with what was read and the reason.
func describeEXIF(h chunkHeader, data io.Reader) ([]format.Field, error) {
	if h.Length > maxChunkRead {
		return []format.Field{{Name: "EXIF", Value: fmt.Sprintf("%d bytes (too large to decode)", h.Length)}}, nil
	}
	b, err := io.ReadAll(data)
	if err != nil {
		return nil, err
	}
	fields, err := exif.Parse(b)
	switch {
	case err != nil:
		fields = append(fields, format.Field{Name: "EXIF", Value: fmt.Sprintf("%d bytes, %v", h.Length, err)})
	case len(fields) == 0:
		fields = append(fields, format.Field{Name: "EXIF", Value: fmt.Sprintf("%d bytes, no tags", h.Length)})
	}
	return fields, nil
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
func parseZText(b []byte) (name, value string, ok bool) {
	key, rest, ok := bytes.Cut(b, []byte{0})
	if !ok || len(rest) < 1 || rest[0] != 0 {
		return "", "", false
	}
	text, err := inflate(rest[1:])
	if err != nil {
		return "", "", false
	}
	return "zTXt " + latin1(key), latin1(text), true
}

// parseIText decodes an iTXt chunk: keyword, NUL, compression flag, method,
// language tag, NUL, translated keyword, NUL, UTF-8 text (zlib if flagged).
func parseIText(b []byte) (name, value string, ok bool) {
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
		if text, err = inflate(text); err != nil {
			return "", "", false
		}
	}
	name = "iTXt " + latin1(key)
	if len(lang) > 0 {
		name += " [" + latin1(lang) + "]"
	}
	return name, string(text), true
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
func inflate(b []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, maxValueLen+1))
}

// latin1 converts ISO 8859-1 bytes to a UTF-8 string.
func latin1(b []byte) string {
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
