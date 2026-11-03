package jpeg

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image"
	stdjpeg "image/jpeg"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"traceless/internal/format"
	"traceless/internal/format/exif/exiftest"
)

var update = flag.Bool("update", false, "rewrite testdata fixtures")

// strip runs Strip and fails the test on error.
func strip(t *testing.T, input []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := (Handler{}).Strip(budget(), bytes.NewReader(input), &out); err != nil {
		t.Fatalf("Strip: %v", err)
	}
	return out.Bytes()
}

func decode(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := stdjpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return img
}

// assertCleanCopy checks that out is a verified, metadata-free file showing
// the same pixels as ref.
func assertCleanCopy(t *testing.T, ref, out []byte) {
	t.Helper()
	var h Handler
	if err := h.Verify(budget(), bytes.NewReader(out)); err != nil {
		t.Errorf("Verify(output): %v", err)
	}
	if fields, err := h.Inspect(budget(), bytes.NewReader(out)); err != nil || len(fields) != 0 {
		t.Errorf("Inspect(output) = %v, %v", fields, err)
	}
	assertSamePixels(t, decode(t, out), decode(t, ref))
	if again := strip(t, out); !bytes.Equal(again, out) {
		t.Error("Strip is not idempotent")
	}
}

func assertSamePixels(t *testing.T, got, want image.Image) {
	t.Helper()
	if got.Bounds() != want.Bounds() {
		t.Fatalf("bounds %v, want %v", got.Bounds(), want.Bounds())
	}
	b := want.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r1, g1, b1, a1 := got.At(x, y).RGBA()
			r2, g2, b2, a2 := want.At(x, y).RGBA()
			if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
				t.Fatalf("pixel (%d,%d) differs", x, y)
			}
		}
	}
}

// hasField reports whether fields contain name with a value containing value.
func hasField(fields []format.Field, name, value string) bool {
	for _, f := range fields {
		if f.Name == name && strings.Contains(f.Value, value) {
			return true
		}
	}
	return false
}

func TestMatch(t *testing.T) {
	var h Handler
	if !h.Match(baseJPEG(t)) {
		t.Error("JPEG not matched")
	}
	if h.Match([]byte("\x89PNG\r\n\x1a\n")) {
		t.Error("PNG matched as JPEG")
	}
	if h.Match([]byte("\xff\xd8")) {
		t.Error("short input matched")
	}
}

func TestInspect(t *testing.T) {
	fields, err := Handler{}.Inspect(budget(), bytes.NewReader(metadataJPEG(t)))
	if err != nil {
		t.Fatal(err)
	}
	want := []format.Field{
		{Name: "EXIF Model", Value: "Pixel 7 Pro"},
		{Name: "EXIF CameraOwnerName", Value: "Jane Doe"},
		{Name: "EXIF GPS position", Value: "52.520000, 13.405000"},
		{Name: "XMP", Value: "<x:xmpmeta>GPS 52.5200,13.4050</x:xmpmeta>"},
		{Name: "ICC profile", Value: "30 bytes"},
		{Name: "IPTC By-line", Value: "Jane Doe"},
		{Name: "IPTC City", Value: "Berlin"},
		{Name: "Photoshop segment", Value: "bytes"},
		{Name: "Comment", Value: "taken at home"},
		{Name: "APP11 segment (PRIVATE)", Value: "23 bytes"},
		{Name: "Trailing data", Value: "14 bytes after EOI"},
	}
	for _, w := range want {
		if !hasField(fields, w.Name, w.Value) {
			t.Errorf("missing %s: %s", w.Name, w.Value)
		}
	}
	for _, f := range fields {
		if !strings.HasPrefix(f.Name, "EXIF ") && !hasField(want, f.Name, "") {
			t.Errorf("unexpected field %s: %s", f.Name, f.Value)
		}
	}
}

func TestInspectClean(t *testing.T) {
	for name, in := range map[string][]byte{
		"standard library": baseJPEG(t),
		"grayscale":        built{width: 16, height: 16}.bytes(),
		"JFIF":             withSegments(baseJPEG(t), app(0, jfifData(0, 0, ""))),
		"restart interval": built{width: 16, height: 16, restart: 1}.bytes(),
	} {
		t.Run(name, func(t *testing.T) {
			fields, err := Handler{}.Inspect(budget(), bytes.NewReader(in))
			if err != nil || len(fields) != 0 {
				t.Errorf("Inspect = %v, %v", fields, err)
			}
		})
	}
}

// gray8 returns an 8x8 grayscale file whose DHT segment is dht and whose
// image data is data.
func gray8(dht []byte, data string) []byte {
	std := seg(markerDHT, appendDHT(appendDHT(nil, 0, 0, stdLumaDC), 1, 0, stdLumaAC))
	return replace(built{width: 8, height: 8, data: []byte(data)}.bytes(), string(std), string(seg(markerDHT, dht)))
}

// Verify accepts only what Strip writes. Encoders have free choices that
// carry no metadata Inspect could show but can still hold data, like a
// Huffman table listing a symbol twice: each use of it picks one of two
// codes, one hidden bit.
func TestVerifyRequiresCanonicalForm(t *testing.T) {
	// DC symbol 0 has codes 0 and 10, end of block has code 0 in its own
	// table: "0 0" and "10 0" are the same zero block.
	dup := appendDHT(appendDHT(nil, 0, 0, huffSpec{[16]byte{1, 1}, []byte{0, 0}}), 1, 0, huffSpec{[16]byte{1}, []byte{0}})
	tests := map[string][]byte{
		"JFIF version 1.02":      withSegments(baseJPEG(t), app(0, jfifData(0, 0, ""))),
		"restart interval":       built{width: 16, height: 16, restart: 1}.bytes(),
		"Adobe flags":            built{width: 8, height: 8, sampling: []byte{0x11, 0x11, 0x11}, segs: [][]byte{app(14, adobeData(0, ""))}}.bytes(),
		"component IDs":          built{width: 8, height: 8, ids: []byte{7}}.bytes(),
		"duplicate Huffman code": gray8(dup, "\x9f"), // codes 10 0, padded with 1s
		"SOF1":                   replace(built{width: 8, height: 8}.bytes(), "\xff\xc0", "\xff\xc1"),
		"split DHT segment": replace(built{width: 8, height: 8}.bytes(),
			string(seg(markerDHT, appendDHT(appendDHT(nil, 0, 0, stdLumaDC), 1, 0, stdLumaAC))),
			string(seg(markerDHT, appendDHT(nil, 0, 0, stdLumaDC)))+string(seg(markerDHT, appendDHT(nil, 1, 0, stdLumaAC)))),
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			var h Handler
			if fields, err := h.Inspect(budget(), bytes.NewReader(in)); err != nil || len(fields) != 0 {
				t.Errorf("Inspect = %v, %v", fields, err)
			}
			if err := h.Verify(budget(), bytes.NewReader(in)); !errors.Is(err, format.ErrMetadataRemains) {
				t.Errorf("Verify = %v, want ErrMetadataRemains", err)
			}
			assertCleanCopy(t, in, strip(t, in))
		})
	}
}

// RGB images get the IDs "R", "G", "B", which some decoders need, and
// identical quantization tables are written once.
func TestStripCanonicalHeaders(t *testing.T) {
	adobeRGB := built{width: 8, height: 8, sampling: []byte{0x11, 0x11, 0x11}, segs: [][]byte{app(14, adobeData(0, ""))}}.bytes()
	out := strip(t, adobeRGB)
	if !bytes.Contains(out, []byte("\xff\xc0\x00\x11\x08\x00\x08\x00\x08\x03R\x11\x00G\x11\x00B\x11\x00")) {
		t.Errorf("RGB frame header not found in % x", out)
	}
	ycc := built{width: 8, height: 8, sampling: []byte{0x11, 0x11, 0x11}, ids: []byte("RGB"), segs: [][]byte{app(14, adobeData(1, ""))}}.bytes()
	if out := strip(t, ycc); !bytes.Contains(out, []byte("\x03\x01\x11\x00\x02\x11\x00\x03\x11\x00")) {
		t.Errorf("YCbCr frame header not found in % x", out)
	}

	// Table 1 is a copy of table 0.
	table := append([]byte{0}, bytes.Repeat([]byte{1}, 64)...)
	twoTables := replace(built{width: 8, height: 8, sampling: []byte{0x11, 0x11, 0x11}}.bytes(),
		"\x02\x11\x00\x03\x11\x00", "\x02\x11\x01\x03\x11\x01")
	twoTables = withSegments(twoTables, seg(markerDQT, append([]byte{1}, table[1:]...)))
	out = strip(t, twoTables)
	if !bytes.Contains(out, seg(markerDQT, table)) || bytes.Count(out, []byte{0xff, markerDQT}) != 1 {
		t.Errorf("tables not merged: % x", out)
	}
	assertCleanCopy(t, twoTables, out)
}

func TestStrip(t *testing.T) {
	in := metadataJPEG(t)
	out := strip(t, in)
	for _, secret := range []string{"Jane", "Berlin", "Pixel", "GPS", "home", "secret", "SECRET", "ICC"} {
		if bytes.Contains(out, []byte(secret)) {
			t.Errorf("output contains %q", secret)
		}
	}
	assertCleanCopy(t, in, out)
}

// The standard library writes the same headers and tables Strip does, so its
// files without padding blocks strip to themselves, byte for byte.
func TestStripKeepsCleanFiles(t *testing.T) {
	for _, in := range [][]byte{
		baseJPEG(t),
		encode(t, testImage(64, 48)),
		built{width: 16, height: 16}.bytes(),
	} {
		if out := strip(t, in); !bytes.Equal(out, in) {
			t.Errorf("%d-byte file changed to %d bytes", len(in), len(out))
		}
	}
}

// Rendering parts are rebuilt from their values: the density stays, the
// version and thumbnail go.
func TestStripRebuildsJFIF(t *testing.T) {
	in := withSegments(baseJPEG(t), app(0, jfifData(2, 1, "")))
	out := strip(t, in)
	want := app(0, prefixJFIF+"\x01\x01\x01\x00\x48\x00\x48\x00\x00")
	if !bytes.Equal(out[2:2+len(want)], want) {
		t.Errorf("JFIF segment = %q, want %q", out[2:2+len(want)], want)
	}
	assertCleanCopy(t, in, out)
}

func TestStripImageKinds(t *testing.T) {
	tests := map[string][]byte{
		"4:2:0 with padding blocks": encode(t, testImage(17, 9)),
		"grayscale odd size":        encode(t, image.NewGray(image.Rect(0, 0, 13, 5))),
		"one pixel":                 encode(t, testImage(1, 1)),
		"4:4:4":                     built{width: 9, height: 9, sampling: []byte{0x11, 0x11, 0x11}}.bytes(),
		"4:2:2":                     built{width: 33, height: 7, sampling: []byte{0x21, 0x11, 0x11}}.bytes(),
		"4:4:0":                     built{width: 7, height: 33, sampling: []byte{0x12, 0x11, 0x11}}.bytes(),
		"grayscale sampled 2x2":     built{width: 20, height: 20, sampling: []byte{0x22}}.bytes(),
		"restart every MCU":         built{width: 40, height: 24, sampling: []byte{0x22, 0x11, 0x11}, restart: 1}.bytes(),
		"restart past RST7":         built{width: 80, height: 8, restart: 1}.bytes(),
		"CMYK":                      built{width: 8, height: 8, sampling: []byte{0x11, 0x11, 0x11, 0x11}, segs: [][]byte{app(14, adobeData(0, ""))}}.bytes(),
		"YCCK":                      built{width: 8, height: 8, sampling: []byte{0x11, 0x11, 0x11, 0x11}, segs: [][]byte{app(14, adobeData(2, ""))}}.bytes(),
		"RGB by Adobe transform":    built{width: 8, height: 8, sampling: []byte{0x11, 0x11, 0x11}, segs: [][]byte{app(14, adobeData(0, ""))}}.bytes(),
		"RGB by component IDs":      built{width: 8, height: 8, ids: []byte("RGB")}.bytes(),
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			assertCleanCopy(t, in, strip(t, in))
		})
	}
}

func TestOrientation(t *testing.T) {
	be := binary.BigEndian
	rotated := exiftest.Build(be, &exiftest.IFD{Entries: []exiftest.Entry{{Tag: 0x0112, Value: []uint16{6}}}})
	upright := exiftest.Build(be, &exiftest.IFD{Entries: []exiftest.Entry{{Tag: 0x0112, Value: []uint16{1}}}})
	tests := []struct {
		name string
		segs [][]byte
		want int // orientation in the output, 0 for no EXIF
	}{
		{"photo", [][]byte{app(1, prefixEXIF, string(exiftest.Photo(be)))}, 6},
		{"little-endian", [][]byte{app(1, prefixEXIF, string(exiftest.Photo(binary.LittleEndian)))}, 6},
		{"only orientation", [][]byte{app(1, prefixEXIF, string(rotated))}, 6},
		{"upright", [][]byte{app(1, prefixEXIF, string(upright))}, 0},
		{"none", nil, 0},
		{"first segment wins", [][]byte{app(1, prefixEXIF, string(upright)), app(1, prefixEXIF, string(rotated))}, 0},
		{"after JFIF", [][]byte{app(0, jfifData(0, 0, "")), app(1, prefixEXIF, string(rotated))}, 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := withSegments(baseJPEG(t), tt.segs...)
			out := strip(t, in)
			var got int
			if i := bytes.Index(out, []byte(prefixEXIF)); i >= 0 {
				got = exifOrientation(out[i:])
				n := int(binary.BigEndian.Uint16(out[i-2:])) - 2
				if !isOrientationSegment(out[i:i+n], got) {
					t.Errorf("EXIF segment holds more than the orientation: %q", out[i:i+n])
				}
			}
			if got != tt.want {
				t.Errorf("orientation %d, want %d", got, tt.want)
			}
			assertCleanCopy(t, in, out)
		})
	}
}

// A file that is just what Strip writes for a rotated photo is clean.
func TestOrientationSegmentIsClean(t *testing.T) {
	in := withSegments(baseJPEG(t), app(1, string(orientationSegment(8))))
	if err := (Handler{}).Verify(budget(), bytes.NewReader(in)); err != nil {
		t.Errorf("Verify: %v", err)
	}
	if out := strip(t, in); !bytes.Equal(out, in) {
		t.Error("Strip changed the file")
	}
}

// TestHiddenData covers places that hold data a decoder never uses. Each must
// be reported by Inspect, rejected by Verify and removed by Strip.
func TestHiddenData(t *testing.T) {
	gray := func(data string, restart int, segs ...[]byte) []byte {
		return built{width: 8 * (1 + min(restart, 1)), height: 8, data: []byte(data), restart: restart, segs: segs}.bytes()
	}
	// One zero block is the codes 00 (DC) and 1010 (end of block), padded
	// with 11: 0x2B.
	tests := []struct {
		name, field, value string
		in                 []byte
	}{
		{"trailing data", "Trailing data", "14 bytes after EOI", append(baseJPEG(t), "SECRET-TRAILER"...)},
		{"appended image", "Trailing data", "(another JPEG image)", append(baseJPEG(t), baseJPEG(t)...)},
		{"bytes after the image data", "Hidden data", "6 bytes after the compressed image data", beforeEOI(baseJPEG(t), "SECRET")},
		{"padding bits", "Hidden data", "padding bits not set in 1 places", gray("\x28", 0)},
		{"padding before a restart", "Hidden data", "padding bits not set in 1 places", gray("\x28\xff\xd0\x2b", 1)},
		{"bytes before a restart", "Hidden data", "6 bytes after the compressed image data", gray("\x2bSECRET\xff\xd0\x2b", 1)},
		{"fill bytes", "Hidden data", "3 stray bytes before markers", withSegments(baseJPEG(t), []byte("\xff\xff\xff"))},
		{"garbage between segments", "Hidden data", "6 stray bytes before markers", withSegments(baseJPEG(t), []byte("SECRET"))},
		{"fill bytes before EOI", "Hidden data", "2 stray bytes before markers", beforeEOI(baseJPEG(t), "\xff\xff")},
		{"padding blocks", "Hidden data", "1 blocks outside the visible image carry data", encode(t, testImage(17, 9))},
		{"grayscale from the standard library", "Hidden data", "1 unused quantization tables",
			encode(t, image.NewGray(image.Rect(0, 0, 16, 16)))},
		{"unused quantization table", "Hidden data", "1 unused quantization tables",
			withSegments(baseJPEG(t), seg(markerDQT, append([]byte{3}, bytes.Repeat([]byte("SECRET!!"), 8)...)))},
		{"replaced quantization table", "Hidden data", "1 unused quantization tables",
			withSegments(baseJPEG(t), seg(markerDQT, append([]byte{0}, bytes.Repeat([]byte("SECRET!!"), 8)...)))},
		{"unused Huffman table", "Hidden data", "1 unused Huffman tables",
			withSegments(baseJPEG(t), seg(markerDHT, appendDHT(nil, 1, 3, huffSpec{[16]byte{0, 0, 0, 0, 0, 0, 6}, []byte("SECRET")})))},
		{"unused restart interval", "Hidden data", "1 unused restart intervals",
			withSegments(baseJPEG(t), seg(markerDRI, []byte("SE")), seg(markerDRI, []byte{0, 0}))},
		{"JFIF thumbnail", "JFIF thumbnail", "2x2 pixels", withSegments(baseJPEG(t), app(0, jfifData(2, 2, "")))},
		{"bytes after JFIF", "Hidden data", "6 bytes after the JFIF header", withSegments(baseJPEG(t), app(0, jfifData(0, 0, "SECRET")))},
		{"bytes after Adobe", "Hidden data", "6 bytes after the Adobe segment", withSegments(baseJPEG(t), app(14, adobeData(1, "SECRET")))},
		{"Adobe in grayscale", "Adobe segment", "unused", built{width: 8, height: 8, segs: [][]byte{app(14, adobeData(0, ""))}}.bytes()},
		{"Adobe overridden by JFIF", "Adobe segment", "unused",
			withSegments(baseJPEG(t), app(0, jfifData(0, 0, "")), app(14, adobeData(0, "")))},
		{"metadata after the image", "Comment", "SECRET", beforeEOI(baseJPEG(t), "\xff\xfe\x00\x08SECRET")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h Handler
			fields, err := h.Inspect(budget(), bytes.NewReader(tt.in))
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if !hasField(fields, tt.field, tt.value) {
				t.Errorf("Inspect = %v, want %s: %s", fields, tt.field, tt.value)
			}
			if err := h.Verify(budget(), bytes.NewReader(tt.in)); !errors.Is(err, format.ErrMetadataRemains) {
				t.Errorf("Verify = %v, want ErrMetadataRemains", err)
			}
			out := strip(t, tt.in)
			if bytes.Contains(out, []byte("SECRET")) {
				t.Error("output contains the secret")
			}
			assertCleanCopy(t, tt.in, out)
		})
	}
}

// Each kind of metadata segment alone must fail Verify.
func TestVerifyFindsMetadata(t *testing.T) {
	segs := map[string][]byte{
		"EXIF":          app(1, prefixEXIF, string(exiftest.Photo(binary.BigEndian))),
		"XMP":           app(1, prefixXMP, "<x/>"),
		"XMP extension": app(1, prefixXMPExt, "x"),
		"ICC":           app(2, prefixICC, "\x01\x01"),
		"MPF":           app(2, prefixMPF, "MM\x00*"),
		"Photoshop":     app(13, photoshopIPTC()),
		"comment":       seg(markerCOM, []byte("hi")),
		"JFXX":          app(0, prefixJFXX, "\x10"),
		"short JFIF":    app(0, prefixJFIF),
		"short Adobe":   app(14, prefixAdobe),
		"empty APP15":   app(15),
	}
	for name, s := range segs {
		t.Run(name, func(t *testing.T) {
			in := withSegments(baseJPEG(t), s)
			if err := (Handler{}).Verify(budget(), bytes.NewReader(in)); !errors.Is(err, format.ErrMetadataRemains) {
				t.Errorf("Verify = %v, want ErrMetadataRemains", err)
			}
			fields, err := Handler{}.Inspect(budget(), bytes.NewReader(in))
			if err != nil || len(fields) == 0 {
				t.Errorf("Inspect = %v, %v", fields, err)
			}
			assertCleanCopy(t, in, strip(t, in))
		})
	}
}

// block codes one block with the luma tables.
func blockData(blk block) string {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	bw := &bitWriter{w: w}
	var pred int32
	bw.encodeBlock(stdEncoders[0], stdEncoders[1], &pred, &blk)
	bw.flush()
	w.Flush()
	return buf.String()
}

// replace returns b with the first occurrence of old replaced.
func replace(b []byte, old, repl string) []byte {
	return bytes.Replace(b, []byte(old), []byte(repl), 1)
}

func TestRejectsInvalidInput(t *testing.T) {
	base := baseJPEG(t)
	gray := built{width: 8, height: 8}.bytes()
	color3 := built{width: 8, height: 8, sampling: []byte{0x11, 0x11, 0x11}}.bytes()
	grayData := func(data string) []byte { return built{width: 8, height: 8, data: []byte(data)}.bytes() }
	sof := "\xff\xc0\x00\x0b\x08\x00\x08\x00\x08\x01\x01\x11\x00"
	sos := "\xff\xda\x00\x08\x01\x01\x00\x00\x3f\x00"

	malformedCases := map[string][]byte{
		"empty":                             nil,
		"SOI only":                          []byte("\xff\xd8"),
		"not a JPEG":                        []byte("\xff\xd9\xff\xd8"),
		"missing EOI":                       base[:len(base)-2],
		"truncated segment":                 base[:30],
		"no image data":                     []byte("\xff\xd8\xff\xd9"),
		"segment length 1":                  withSegments(base, []byte("\xff\xe1\x00\x01")),
		"second SOI":                        withSegments(base, []byte("\xff\xd8")),
		"restart outside scan":              withSegments(base, []byte("\xff\xd0")),
		"scan before frame":                 replace(gray, sof, ""),
		"two frames":                        replace(gray, sof, sof+sof),
		"undefined table":                   replace(gray, sos, "\xff\xda\x00\x08\x01\x01\x22\x00\x3f\x00"),
		"undefined quant table":             replace(gray, sof, "\xff\xc0\x00\x0b\x08\x00\x08\x00\x08\x01\x01\x11\x01"),
		"baseline table 2":                  replace(gray, sos, "\xff\xda\x00\x08\x01\x01\x20\x00\x3f\x00"),
		"wrong scan component":              replace(gray, sos, "\xff\xda\x00\x08\x01\x07\x00\x00\x3f\x00"),
		"spectral selection":                replace(gray, sos, "\xff\xda\x00\x08\x01\x01\x00\x00\x3e\x00"),
		"SOS length":                        replace(gray, sos, "\xff\xda\x00\x09\x01\x01\x00\x00\x3f\x00\x00"),
		"SOF length":                        replace(gray, sof, "\xff\xc0\x00\x0c\x08\x00\x08\x00\x08\x01\x01\x11\x00\x00"),
		"width 0":                           replace(gray, sof, "\xff\xc0\x00\x0b\x08\x00\x08\x00\x00\x01\x01\x11\x00"),
		"sampling 0":                        replace(gray, sof, "\xff\xc0\x00\x0b\x08\x00\x08\x00\x08\x01\x01\x01\x00"),
		"duplicate component":               replace(color3, "\x02\x11\x00", "\x01\x11\x00"),
		"too many blocks":                   built{width: 8, height: 8, sampling: []byte{0x44, 0x11, 0x11}, data: []byte{}}.bytes(),
		"quantization value 0":              replace(gray, "\xff\xdb\x00\x43\x00\x01", "\xff\xdb\x00\x43\x00\x00"),
		"truncated DQT":                     replace(gray, "\xff\xdb\x00\x43", "\xff\xdb\x00\x42"),
		"oversubscribed table":              withSegments(gray, seg(markerDHT, appendDHT(nil, 0, 2, huffSpec{[16]byte{3}, []byte{0, 1, 2}}))),
		"bad DRI length":                    withSegments(gray, seg(markerDRI, []byte{0, 1, 0})),
		"data ends inside block":            grayData(""),
		"invalid Huffman code":              grayData("\xff\x00\xff\x00"),
		"DC out of range":                   grayData(blockData(block{1500})),
		"wrong restart marker":              built{width: 16, height: 8, restart: 1, data: []byte("\x2b\xff\xd1\x2b")}.bytes(),
		"missing restart marker":            built{width: 16, height: 8, restart: 1, data: []byte("\x2b\x2b")}.bytes(),
		"duplicate JFIF":                    withSegments(base, app(0, jfifData(0, 0, "")), app(0, jfifData(0, 0, ""))),
		"JFIF units":                        withSegments(base, app(0, prefixJFIF+"\x01\x02\x07\x00\x01\x00\x01\x00\x00")),
		"truncated thumbnail":               withSegments(base, app(0, prefixJFIF+"\x01\x02\x00\x00\x01\x00\x01\x02\x02")),
		"duplicate Adobe":                   withSegments(base, app(14, adobeData(1, "")), app(14, adobeData(1, ""))),
		"Adobe transform 3":                 withSegments(base, app(14, adobeData(3, ""))),
		"YCCK with 3 components":            withSegments(base, app(14, adobeData(2, ""))),
		"stuffed fill bytes":                grayData("\xff\xff\x00"),
		"all-ones Huffman code":             gray8(appendDHT(appendDHT(nil, 0, 0, huffSpec{[16]byte{2}, []byte{0, 1}}), 1, 0, stdLumaAC), "\x2b"),
		"DC and AC in one progressive scan": replace(gray, "\xff\xc0", "\xff\xc2"),
		"DC symbol 16":                      gray8(appendDHT(appendDHT(nil, 0, 0, huffSpec{[16]byte{1}, []byte{16}}), 1, 0, stdLumaAC), "\x2b"),
	}
	unsupportedCases := map[string][]byte{
		"arithmetic":         replace(gray, "\xff\xc0", "\xff\xc9"),
		"lossless":           replace(gray, "\xff\xc0", "\xff\xc3"),
		"12-bit":             replace(gray, sof, "\xff\xc0\x00\x0b\x0c\x00\x08\x00\x08\x01\x01\x11\x00"),
		"height from DNL":    replace(gray, sof, "\xff\xc0\x00\x0b\x08\x00\x00\x00\x08\x01\x01\x11\x00"),
		"two components":     built{width: 8, height: 8, sampling: []byte{0x11, 0x11}, data: []byte{}}.bytes(),
		"missing components": replace(color3, "\xff\xda\x00\x0c\x03\x01\x00\x02\x11\x03\x11", "\xff\xda\x00\x08\x01\x01\x00"),
		"DAC":                withSegments(gray, seg(markerDAC, []byte{0, 0})),
		"reserved marker":    withSegments(gray, seg(0xf0, nil)),
		"segment after EOI":  beforeEOI(gray, string(seg(markerDQT, append([]byte{0}, bytes.Repeat([]byte{1}, 64)...)))),
	}
	check := func(t *testing.T, in []byte, want error) {
		t.Helper()
		var h Handler
		if _, err := h.Inspect(budget(), bytes.NewReader(in)); !errors.Is(err, want) {
			t.Errorf("Inspect = %v, want %v", err, want)
		}
		// Verify stops at the first scan Strip wouldn't write, which can
		// come before the broken part.
		if err := h.Verify(budget(), bytes.NewReader(in)); !errors.Is(err, want) && !errors.Is(err, format.ErrMetadataRemains) {
			t.Errorf("Verify = %v, want %v", err, want)
		}
		if err := h.Strip(budget(), bytes.NewReader(in), io.Discard); !errors.Is(err, want) {
			t.Errorf("Strip = %v, want %v", err, want)
		}
	}
	for name, in := range malformedCases {
		t.Run(name, func(t *testing.T) { check(t, in, format.ErrMalformed) })
	}
	for name, in := range unsupportedCases {
		t.Run(name, func(t *testing.T) { check(t, in, format.ErrUnsupported) })
	}
}

type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// Read errors other than a short read are returned unchanged, so callers can
// tell a broken file from a failing disk.
func TestReadErrors(t *testing.T) {
	in := metadataJPEG(t)
	for _, n := range []int{1, 10, 200, len(in) / 2, len(in) - 1} {
		var h Handler
		_, err := h.Inspect(budget(), &failingReader{in[:n], context.Canceled})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Inspect after %d bytes = %v, want context.Canceled", n, err)
		}
		err = h.Strip(budget(), &failingReader{in[:n], context.Canceled}, io.Discard)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Strip after %d bytes = %v, want context.Canceled", n, err)
		}
	}
}

// Each block costs at least two bits of input, so a large image of tiny
// blocks is bounded by its input size: Strip's work and output grow with it.
func TestCheapBlocks(t *testing.T) {
	const size = 2048
	// One-symbol tables: every block is the bits 0 (DC 0) and 0 (end of
	// block).
	one := huffSpec{[16]byte{1}, []byte{0}}
	dht := appendDHT(appendDHT(nil, 0, 0, one), 1, 0, one)
	in := built{width: size, height: size, data: bytes.Repeat([]byte{0}, size*size/64/4)}.bytes()
	in = replace(in, string(seg(markerDHT, appendDHT(appendDHT(nil, 0, 0, stdLumaDC), 1, 0, stdLumaAC))), string(seg(markerDHT, dht)))

	start := time.Now()
	var out bytes.Buffer
	if err := (Handler{}).Strip(budget(), bytes.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Strip took %v", d)
	}
	if out.Len() > 4*len(in) {
		t.Errorf("%d bytes of input became %d", len(in), out.Len())
	}
	assertCleanCopy(t, in, out.Bytes())
}

// Many segments must not produce an unbounded number of fields, and long
// values are clipped.
func TestInspectLimits(t *testing.T) {
	var segs [][]byte
	for range 5000 {
		segs = append(segs, app(9, "x"))
	}
	for i := range 2 * format.MaxFields {
		segs = append(segs, seg(markerCOM, []byte(fmt.Sprint(i))))
	}
	segs = append(segs, app(1, prefixXMP, strings.Repeat("\xe9", 60000)))
	fields, err := Handler{}.Inspect(budget(), bytes.NewReader(withSegments(baseJPEG(t), segs...)))
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != format.MaxFields+1 {
		t.Errorf("got %d fields, want %d", len(fields), format.MaxFields+1)
	}
	if !hasField(fields, "APP9 segment", "5000 segments, 5000 bytes") {
		t.Errorf("APP9 segments not grouped: %v", fields[0])
	}

	in := withSegments(baseJPEG(t), app(1, prefixXMP, strings.Repeat("\xe9", 60000)))
	fields, err = Handler{}.Inspect(budget(), bytes.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(fields[0].Value); n > maxValueLen+64 {
		t.Errorf("value is %d bytes, want at most about %d", n, maxValueLen)
	}
}

// Long values are cut at a character boundary without losing their
// encoding.
func TestText(t *testing.T) {
	long := strings.Repeat("ä", maxValueLen)
	if got := text([]byte(long)); !utf8.ValidString(got) || !strings.HasPrefix(got, "ää") || !strings.HasSuffix(got, "(truncated)") {
		t.Errorf("UTF-8: %q...", got[:10])
	}
	if got := text([]byte("caf\xe9")); got != "café" {
		t.Errorf("Latin-1: %q", got)
	}
}

// Only the first EXIF segment is decoded; repeats are grouped by size.
func TestInspectDecodesOneEXIF(t *testing.T) {
	e := app(1, prefixEXIF, string(exiftest.Photo(binary.BigEndian)))
	fields, err := Handler{}.Inspect(budget(), bytes.NewReader(withSegments(baseJPEG(t), e, e, e)))
	if err != nil {
		t.Fatal(err)
	}
	if !hasField(fields, "EXIF segment", "2 segments") {
		t.Errorf("repeated EXIF not grouped: %v", fields)
	}
}

// TestFixture keeps testdata/metadata.jpg, a file with known metadata used by
// the fuzz corpus and the leak checker. Regenerate it with -update.
// Verify compares the input with the rebuilt file as both stream; a stripped
// file larger than what the comparison may hold must still pass.
func TestVerifyLargeImage(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	rng := rand.New(rand.NewPCG(1, 2))
	img := image.NewRGBA(image.Rect(0, 0, 1600, 1600))
	for i := range img.Pix {
		img.Pix[i] = byte(rng.Uint32())
	}
	var buf bytes.Buffer
	if err := stdjpeg.Encode(&buf, img, &stdjpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	out := strip(t, buf.Bytes())
	if len(out) <= 4<<20 { // format.maxLag
		t.Fatalf("stripped file has only %d bytes", len(out))
	}
	if err := (Handler{}).Verify(budget(), bytes.NewReader(out)); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestFixture(t *testing.T) {
	path := filepath.Join("testdata", "metadata.jpg")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, metadataJPEG(t), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run TestFixture -update)", err)
	}
	if !bytes.Equal(data, metadataJPEG(t)) {
		t.Error("fixture is out of date (run go test -run TestFixture -update)")
	}
}

// Each method spends from its budget and stops with ErrLimit when it runs
// out, and gives back the memory it held once done.
func TestBudget(t *testing.T) {
	limited := func(l format.Limits) *format.Budget { return format.NewBudget(context.Background(), l) }
	base, prog := readScans(t, "base.jpg"), readScans(t, "prog.jpg")
	tests := []struct {
		name   string
		input  []byte
		limits format.Limits
	}{
		{"blocks of a streamed scan", base, format.Limits{Memory: 1 << 30, Decompressed: 1 << 30, Work: 5}},
		{"blocks of buffered scans", prog, format.Limits{Memory: 1 << 30, Decompressed: 1 << 30, Work: 50}},
		{"coefficients", prog, format.Limits{Memory: 1000, Decompressed: 1 << 30, Work: 1 << 30}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h Handler
			if _, err := h.Inspect(limited(tt.limits), bytes.NewReader(tt.input)); !errors.Is(err, format.ErrLimit) {
				t.Errorf("Inspect = %v, want ErrLimit", err)
			}
			if err := h.Strip(limited(tt.limits), bytes.NewReader(tt.input), io.Discard); !errors.Is(err, format.ErrLimit) {
				t.Errorf("Strip = %v, want ErrLimit", err)
			}
		})
	}

	// One budget that fits one call at a time fits many calls in a row.
	b := limited(format.Limits{Memory: 64 << 10, Decompressed: 1 << 30, Work: 1 << 30})
	for i := range 3 {
		var h Handler
		if err := h.Strip(b, bytes.NewReader(prog), io.Discard); err != nil {
			t.Fatalf("Strip %d: %v", i, err)
		}
	}
}

// A canceled call stops in the image data even when its reader doesn't check
// the context.
func TestBudgetCanceled(t *testing.T) {
	var buf bytes.Buffer
	if err := stdjpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 2100, 2100)), nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Handler{}.Strip(format.NewBudget(ctx, format.DefaultLimits), bytes.NewReader(buf.Bytes()), io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Strip = %v, want context.Canceled", err)
	}
}
