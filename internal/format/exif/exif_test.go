package exif

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"traceless/internal/format"
	"traceless/internal/format/exif/exiftest"
)

func fieldMap(fields []format.Field) map[string]string {
	m := make(map[string]string, len(fields))
	for _, f := range fields {
		m[f.Name] = f.Value
	}
	return m
}

func TestParse(t *testing.T) {
	for _, order := range []exiftest.Order{binary.BigEndian, binary.LittleEndian} {
		t.Run(order.String(), func(t *testing.T) {
			fields, err := Parse(exiftest.Photo(order))
			if err != nil {
				t.Fatal(err)
			}
			got := fieldMap(fields)
			want := map[string]string{
				"EXIF Make":                  "Google",
				"EXIF Model":                 "Pixel 7 Pro",
				"EXIF Orientation":           "6",
				"EXIF Software":              "HDR+ 1.0.540104767zd",
				"EXIF DateTime":              "2024:05:01 12:30:00",
				"EXIF ExposureTime":          "0.008",
				"EXIF FNumber":               "1.85",
				"EXIF ISO":                   "100",
				"EXIF DateTimeOriginal":      "2024:05:01 12:30:00",
				"EXIF UserComment":           "taken at home",
				"EXIF CameraOwnerName":       "Jane Doe",
				"EXIF BodySerialNumber":      "SN-1234567",
				"EXIF GPS position":          "52.520000, 13.405000",
				"EXIF GPSAltitude":           "34",
				"EXIF Thumbnail Compression": "6",
				"EXIF Thumbnail":             "5120 bytes",
			}
			for name, value := range want {
				if got[name] != value {
					t.Errorf("%s = %q, want %q", name, got[name], value)
				}
			}
			if len(got) != len(want) {
				t.Errorf("got %d fields, want %d: %v", len(got), len(want), got)
			}
		})
	}
}

func TestParseText(t *testing.T) {
	utf16le := []byte{'J', 0, 'a', 0, 'n', 0, 'e', 0, 0, 0}
	tests := []struct {
		name  string
		entry exiftest.Entry
		field string
		want  string
	}{
		{"text after NUL is shown", exiftest.Entry{Tag: 0x010e, Value: "photo\x00hidden"}, "EXIF ImageDescription", "photo\x00hidden"},
		{"invalid UTF-8", exiftest.Entry{Tag: 0x013b, Value: "Jane \xff"}, "EXIF Artist", "Jane \uFFFD"},
		{"long text is clipped", exiftest.Entry{Tag: 0x013b, Value: strings.Repeat("a", 5000)}, "EXIF Artist", strings.Repeat("a", maxValueLen) + "… (truncated)"},
		{"Windows author", exiftest.Entry{Tag: 0x9c9d, Value: utf16le}, "EXIF XPAuthor", "Jane"},
		{"binary undefined", exiftest.Entry{Tag: 0x927c, Value: []byte{1, 2, 3, 0xff, 5}}, "EXIF MakerNote", "5 bytes"},
		{"empty comment", exiftest.Entry{Tag: 0x9286, Value: []byte("ASCII\x00\x00\x00   ")}, "EXIF UserComment", "(empty)"},
		{"unknown tag", exiftest.Entry{Tag: 0xbeef, Value: "secret"}, "EXIF Tag 0xBEEF", "secret"},
		{"many values", exiftest.Entry{Tag: 0x0102, Value: make([]uint16, 20)}, "EXIF BitsPerSample", strings.Repeat("0 ", 15) + "0 … (20 values)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := exiftest.Build(binary.BigEndian, &exiftest.IFD{Entries: []exiftest.Entry{tt.entry}})
			fields, err := Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			if got := fieldMap(fields)[tt.field]; got != tt.want {
				t.Errorf("%s = %q, want %q", tt.field, got, tt.want)
			}
		})
	}
}

func TestParseUnicodeComment(t *testing.T) {
	for _, order := range []exiftest.Order{binary.BigEndian, binary.LittleEndian} {
		comment := []byte("UNICODE\x00")
		for _, r := range "hi" {
			comment = order.AppendUint16(comment, uint16(r))
		}
		data := exiftest.Build(order, &exiftest.IFD{Entries: []exiftest.Entry{{Tag: 0x9286, Value: comment}}})
		fields, err := Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		if got := fieldMap(fields)["EXIF UserComment"]; got != "hi" {
			t.Errorf("%v: UserComment = %q, want %q", order, got, "hi")
		}
	}
}

// A position that can't be decoded is still reported, tag by tag.
func TestParseBadGPS(t *testing.T) {
	data := exiftest.Build(binary.BigEndian, &exiftest.IFD{Sub: map[uint16]*exiftest.IFD{
		0x8825: {Entries: []exiftest.Entry{
			{Tag: 0x01, Value: "N"},
			{Tag: 0x02, Value: [][2]uint32{{52, 1}, {31, 0}, {12, 1}}},
			{Tag: 0x03, Value: "E"},
		}},
	}})
	fields, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	got := fieldMap(fields)
	want := map[string]string{
		"EXIF GPSLatitudeRef":  "N",
		"EXIF GPSLatitude":     "52 31/0 12",
		"EXIF GPSLongitudeRef": "E",
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %q, want %q", name, got[name], value)
		}
	}
}

func TestParseLimitsFields(t *testing.T) {
	var entries []exiftest.Entry
	for i := range 300 {
		entries = append(entries, exiftest.Entry{Tag: uint16(0xc000 + i), Value: []uint16{1}})
	}
	fields, err := Parse(exiftest.Build(binary.BigEndian, &exiftest.IFD{Entries: entries}))
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != maxFields+1 {
		t.Fatalf("got %d fields, want %d", len(fields), maxFields+1)
	}
	if last := fields[len(fields)-1]; last.Value != fmt.Sprintf("%d further tags not shown", 300-maxFields) {
		t.Errorf("last field = %v", last)
	}
}

func TestParseMalformed(t *testing.T) {
	be := binary.BigEndian
	photo := exiftest.Photo(be)

	// IFD0 links back to itself.
	loop := exiftest.Photo(be)
	n := int(be.Uint16(loop[8:]))
	be.PutUint32(loop[8+2+12*n:], 8)

	// A text value whose offset points past the end.
	outside := exiftest.Build(be, &exiftest.IFD{Entries: []exiftest.Entry{{Tag: 0x010f, Value: "Google LLC"}}})
	be.PutUint32(outside[8+2+8:], 1<<31)

	// More entries than allowed.
	huge := []byte("MM\x00*\x00\x00\x00\x08\xff\xff")

	// A chain of more IFDs than allowed.
	chain := &exiftest.IFD{}
	for range maxIFDs {
		chain = &exiftest.IFD{Next: chain}
	}

	tests := []struct {
		name       string
		data       []byte
		wantFields bool // fields read before the problem are returned
	}{
		{"empty", nil, false},
		{"bad header", []byte("XX\x00*\x00\x00\x00\x08"), false},
		{"IFD offset out of range", []byte("MM\x00*\x00\x00\xff\xff"), false},
		{"truncated IFD", photo[:20], false},
		{"IFD loop", loop, true},
		{"value out of range", outside, false},
		{"too many entries", huge, false},
		{"too many IFDs", exiftest.Build(be, chain), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields, err := Parse(tt.data)
			if !errors.Is(err, format.ErrMalformed) {
				t.Errorf("Parse = %v, want ErrMalformed", err)
			}
			if tt.wantFields && len(fields) == 0 {
				t.Error("fields read before the error are missing")
			}
		})
	}
}

// sharedValue returns EXIF with maxIFDs IFDs of maxEntries entries each, all
// pointing at the same value: IFD0 links to the others through SubIFDs.
func sharedValue(typ uint16, value []byte) []byte {
	be := binary.BigEndian
	const table = 2 + 12*maxEntries + 4
	array := uint32(8 + maxIFDs*table)
	valueAt := array + 4*(maxIFDs-1)

	b := []byte("MM\x00*\x00\x00\x00\x08")
	for i := range maxIFDs {
		b = be.AppendUint16(b, maxEntries)
		for j := range maxEntries {
			if i == 0 && j == 0 {
				b = be.AppendUint16(b, 0x014a)
				b = be.AppendUint16(b, typeLong)
				b = be.AppendUint32(b, maxIFDs-1)
				b = be.AppendUint32(b, array)
				continue
			}
			b = be.AppendUint16(b, 0x010e)
			b = be.AppendUint16(b, typ)
			b = be.AppendUint32(b, uint32(len(value)))
			b = be.AppendUint32(b, valueAt)
		}
		b = be.AppendUint32(b, 0)
	}
	for i := 1; i < maxIFDs; i++ {
		b = be.AppendUint32(b, uint32(8+i*table))
	}
	return append(b, value...)
}

// Many tags pointing at one large value must not make Parse scan all of it
// for each tag. Unbounded, these inputs take over ten seconds; bounded, a few
// milliseconds.
func TestParseSharedValues(t *testing.T) {
	tests := map[string][]byte{
		"text of NULs":        sharedValue(typeASCII, make([]byte, 1<<20)),
		"printable undefined": sharedValue(typeUndefined, bytes.Repeat([]byte("A"), 1<<20)),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			fields, err := Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			if len(fields) != maxFields+1 {
				t.Errorf("got %d fields, want %d", len(fields), maxFields+1)
			}
			if d := time.Since(start); d > time.Second {
				t.Errorf("Parse took %v", d)
			}
		})
	}
}

// Pointers must not queue more IFDs than Parse may visit: IFD0 plus 32
// SubIFDs is one too many, even before any of them is read.
func TestParseLimitsQueue(t *testing.T) {
	offsets := make([]uint32, maxIFDs)
	for i := range offsets {
		offsets[i] = uint32(1000 + 2*i)
	}
	data := exiftest.Build(binary.BigEndian, &exiftest.IFD{Entries: []exiftest.Entry{{Tag: 0x014a, Value: offsets}}})
	if _, err := Parse(data); !errors.Is(err, format.ErrMalformed) {
		t.Errorf("Parse = %v, want ErrMalformed", err)
	}
}

// Invalid bytes grow when replaced, so values are clipped after replacement.
func TestParseClipsInvalidUTF8(t *testing.T) {
	data := exiftest.Build(binary.BigEndian, &exiftest.IFD{Entries: []exiftest.Entry{
		{Tag: 0x013b, Value: string(bytes.Repeat([]byte{0xff, 'a'}, 1500))},
	}})
	fields, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	v := fieldMap(fields)["EXIF Artist"]
	if len(v) > maxValueLen+len("… (truncated)") || !utf8.ValidString(v) || !strings.HasSuffix(v, "(truncated)") {
		t.Errorf("value of %d bytes: %q...", len(v), v[:min(len(v), 20)])
	}
}

// FuzzParse checks that Parse never panics and keeps its output bounded.
func FuzzParse(f *testing.F) {
	f.Add(exiftest.Photo(binary.BigEndian))
	f.Add(exiftest.Photo(binary.LittleEndian))
	f.Fuzz(func(t *testing.T, data []byte) {
		fields, _ := Parse(data)
		if len(fields) > maxFields+1 {
			t.Fatalf("%d fields", len(fields))
		}
		for _, f := range fields {
			if len(f.Value) > maxValueLen+64 || !utf8.ValidString(f.Value) {
				t.Fatalf("bad value %q", f.Value)
			}
		}
	})
}

func TestOrientation(t *testing.T) {
	be := binary.BigEndian
	tests := []struct {
		name string
		data []byte
		want int
	}{
		{"big-endian photo", exiftest.Photo(be), 6},
		{"little-endian photo", exiftest.Photo(binary.LittleEndian), 6},
		{"built", OrientationOnly(8), 8},
		{"missing", exiftest.Build(be, &exiftest.IFD{Entries: []exiftest.Entry{{Tag: 0x010f, Value: "Google"}}}), 0},
		{"out of range", exiftest.Build(be, &exiftest.IFD{Entries: []exiftest.Entry{{Tag: 0x0112, Value: []uint16{9}}}}), 0},
		{"wrong type", exiftest.Build(be, &exiftest.IFD{Entries: []exiftest.Entry{{Tag: 0x0112, Value: []uint32{6}}}}), 0},
		{"two values", exiftest.Build(be, &exiftest.IFD{Entries: []exiftest.Entry{{Tag: 0x0112, Value: []uint16{6, 6}}}}), 0},
		{"only in IFD1", exiftest.Build(be, &exiftest.IFD{Next: &exiftest.IFD{Entries: []exiftest.Entry{{Tag: 0x0112, Value: []uint16{6}}}}}), 0},
		{"truncated", exiftest.Photo(be)[:20], 0},
		{"empty", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Orientation(tt.data); got != tt.want {
				t.Errorf("Orientation = %d, want %d", got, tt.want)
			}
		})
	}
}

// The built block must decode to exactly one field.
func TestOrientationOnly(t *testing.T) {
	fields, err := Parse(OrientationOnly(3))
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 || fields[0] != (format.Field{Name: "EXIF Orientation", Value: "3"}) {
		t.Errorf("Parse = %v", fields)
	}
}
