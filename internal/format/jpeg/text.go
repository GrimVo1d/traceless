package jpeg

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf8"

	"traceless/internal/format"
	"traceless/internal/format/exif"
)

const (
	// maxValueLen caps the length of a reported value. Segments are at most
	// 64 KB, but an XMP packet is too long to show whole.
	maxValueLen = 4096

	// maxIdentLen caps the identifier shown for unknown APPn segments.
	maxIdentLen = 32
)

// Known segment kinds, by marker and identifier.
var segmentKinds = []struct {
	marker byte
	prefix string
	name   string
}{
	{markerAPP0, prefixJFIF, "JFIF segment"},
	{markerAPP0, prefixJFXX, "JFIF thumbnail"},
	{markerAPP0 + 1, prefixEXIF, "EXIF segment"},
	{markerAPP0 + 1, prefixXMP, "XMP segment"},
	{markerAPP0 + 1, prefixXMPExt, "XMP extension"},
	{markerAPP0 + 2, prefixICC, "ICC profile"},
	{markerAPP0 + 2, prefixMPF, "Multi-picture index (MPF)"},
	{markerAPP0 + 13, prefixPhotoshop, "Photoshop segment"},
	{markerAPP0 + 14, prefixAdobe, "Adobe segment"},
}

// label names a segment for Verify errors and generic Inspect fields.
func label(m byte, data []byte) string {
	if m == markerCOM {
		return "Comment"
	}
	for _, k := range segmentKinds {
		if m == k.marker && strings.HasPrefix(string(data), k.prefix) {
			return k.name
		}
	}
	name := markerName(m) + " segment"
	if id := ident(data); id != "" {
		name += " (" + id + ")"
	}
	return name
}

// ident returns the identifier an APPn segment starts with: printable ASCII
// up to a NUL byte.
func ident(data []byte) string {
	id, _, ok := bytes.Cut(data[:min(len(data), maxIdentLen+1)], []byte{0})
	if !ok || len(id) == 0 || len(id) > maxIdentLen {
		return ""
	}
	for _, c := range id {
		if c < 0x20 || c > 0x7e {
			return ""
		}
	}
	return string(id)
}

// describe reports a dropped segment. EXIF, XMP, IPTC and comments are
// decoded; other segments are grouped by kind with their sizes.
func (p *processor) describe(m byte, data []byte) {
	add := func(name, value string) {
		p.report.Add(format.Field{Name: name, Value: value}, false, 0)
	}
	switch {
	case p.report.Full():
		// It would only be counted, so don't decode it.
	case m == markerCOM:
		add("Comment", text(data))
		return
	case m == markerAPP0+1 && !p.sawEXIF && strings.HasPrefix(string(data), prefixEXIF):
		// Only the first EXIF segment is decoded, so repeating a costly
		// segment can't multiply the work.
		p.sawEXIF = true
		fields, err := exif.Parse(data[len(prefixEXIF):])
		switch {
		case err != nil:
			fields = append(fields, format.Field{Name: "EXIF", Value: fmt.Sprintf("%d bytes, %v", len(data), err)})
		case len(fields) == 0:
			fields = append(fields, format.Field{Name: "EXIF", Value: fmt.Sprintf("%d bytes, no tags", len(data))})
		}
		for _, f := range fields {
			p.report.Add(f, false, 0)
		}
		return
	case m == markerAPP0+1 && !p.sawXMP && strings.HasPrefix(string(data), prefixXMP):
		p.sawXMP = true
		// XMP packets are padded with whitespace for in-place editing;
		// collapsing it leaves room for the content.
		add("XMP", text(bytes.Join(bytes.Fields(data[len(prefixXMP):]), []byte(" "))))
		return
	case m == markerAPP0+13 && strings.HasPrefix(string(data), prefixPhotoshop):
		for _, f := range iptc(data[len(prefixPhotoshop):]) {
			p.report.Add(f, false, 0)
		}
	}
	p.report.Add(format.Field{Name: label(m, data), Value: fmt.Sprintf("%d bytes", len(data))}, true, int64(len(data)))
}

// iptcNames names the IPTC IIM datasets of record 2 (application) that are
// most often set.
var iptcNames = map[byte]string{
	5: "ObjectName", 7: "EditStatus", 10: "Urgency", 15: "Category",
	20: "SupplementalCategories", 25: "Keywords", 40: "SpecialInstructions",
	55: "DateCreated", 60: "TimeCreated", 62: "DigitalCreationDate",
	63: "DigitalCreationTime", 65: "OriginatingProgram", 70: "ProgramVersion",
	80: "By-line", 85: "By-lineTitle", 90: "City", 92: "Sub-location",
	95: "Province-State", 100: "Country-PrimaryLocationCode",
	101: "Country-PrimaryLocationName", 103: "OriginalTransmissionReference",
	105: "Headline", 110: "Credit", 115: "Source", 116: "CopyrightNotice",
	118: "Contact", 120: "Caption-Abstract", 122: "Writer-Editor",
}

// iptc returns the IPTC fields in Photoshop image resources: a list of
// "8BIM" blocks, where resource 0x0404 holds IPTC IIM records. Anything that
// doesn't parse ends the list; the segment is reported by size anyway.
func iptc(data []byte) []format.Field {
	var fields []format.Field
	for len(data) >= 7 && string(data[:4]) == "8BIM" {
		id := binary.BigEndian.Uint16(data[4:])
		// A Pascal-string name, padded to an even length.
		name := 1 + int(data[6])
		name += name % 2
		if len(data) < 6+name+4 {
			break
		}
		size := uint64(binary.BigEndian.Uint32(data[6+name:]))
		body := data[6+name+4:]
		if size > uint64(len(body)) {
			break
		}
		if id == 0x0404 {
			fields = append(fields, iim(body[:size])...)
			if len(fields) > format.MaxFields {
				break
			}
		}
		data = body[min(size+size%2, uint64(len(body))):]
	}
	return fields
}

// iim decodes IPTC IIM datasets: 0x1C, record, dataset, 16-bit size, data.
func iim(data []byte) []format.Field {
	var fields []format.Field
	for len(data) >= 5 && data[0] == 0x1c {
		record, dataset := data[1], data[2]
		size := int(binary.BigEndian.Uint16(data[3:]))
		if size&0x8000 != 0 || len(data) < 5+size {
			break // extended sizes are only used for binary data
		}
		value := data[5 : 5+size]
		data = data[5+size:]
		if record != 2 || dataset == 0 {
			continue // envelope data and the record version
		}
		name, ok := iptcNames[dataset]
		if !ok {
			name = fmt.Sprintf("2:%d", dataset)
		}
		fields = append(fields, format.Field{Name: "IPTC " + name, Value: text(value)})
		if len(fields) > format.MaxFields {
			break // the report would only count the rest
		}
	}
	return fields
}

// text decodes b as UTF-8, or as Latin-1 if it isn't valid UTF-8, and clips
// it to maxValueLen bytes.
func text(b []byte) string {
	s := string(b)
	if !utf8.Valid(b) {
		var sb strings.Builder
		for _, c := range b {
			sb.WriteRune(rune(c))
		}
		s = sb.String()
	}
	if len(s) <= maxValueLen {
		return s
	}
	cut := maxValueLen
	for !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "… (truncated)"
}
