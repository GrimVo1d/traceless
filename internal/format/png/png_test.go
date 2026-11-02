package png

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image"
	stdpng "image/png"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"traceless/internal/format"
)

var update = flag.Bool("update", false, "rewrite testdata fixtures")

func fieldMap(fields []format.Field) map[string]string {
	m := make(map[string]string, len(fields))
	for _, f := range fields {
		m[f.Name] = f.Value
	}
	return m
}

// strip runs Strip and fails the test on error.
func strip(t *testing.T, input []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := (Handler{}).Strip(bytes.NewReader(input), &out); err != nil {
		t.Fatalf("Strip: %v", err)
	}
	return out.Bytes()
}

func decode(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := stdpng.Decode(bytes.NewReader(b))
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
	if err := h.Verify(bytes.NewReader(out)); err != nil {
		t.Errorf("Verify(output): %v", err)
	}
	if fields, err := h.Inspect(bytes.NewReader(out)); err != nil || len(fields) != 0 {
		t.Errorf("Inspect(output) = %v, %v", fields, err)
	}
	assertSamePixels(t, decode(t, out), decode(t, ref))
	if again := strip(t, out); !bytes.Equal(again, out) {
		t.Error("Strip is not idempotent")
	}
}

func TestMatch(t *testing.T) {
	var h Handler
	if !h.Match(basePNG(t)) {
		t.Error("PNG not matched")
	}
	if h.Match([]byte("\xff\xd8\xff\xe0JFIF")) {
		t.Error("JPEG matched as PNG")
	}
	if h.Match([]byte("\x89PN")) {
		t.Error("short input matched")
	}
}

func TestInspect(t *testing.T) {
	fields, err := Handler{}.Inspect(bytes.NewReader(metadataPNG(t)))
	if err != nil {
		t.Fatal(err)
	}
	got := fieldMap(fields)
	want := map[string]string{
		"ICC profile":                 "Pixel 7 Pro camera (",
		"tEXt Author":                 "Jane Doe",
		"zTXt Comment":                "taken at home",
		"iTXt XML:com.adobe.xmp [en]": "<x:xmpmeta>GPS 52.5200,13.4050</x:xmpmeta>",
		"EXIF":                        "10 bytes",
		"Modified":                    "2024-05-01 12:30:00 UTC",
		"prVt chunk":                  "16 bytes",
		"Trailing data":               "14 bytes after IEND",
	}
	for name, value := range want {
		if !strings.HasPrefix(got[name], value) {
			t.Errorf("%s = %q, want prefix %q", name, got[name], value)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d fields, want %d: %v", len(got), len(want), got)
	}
}

func TestInspectClean(t *testing.T) {
	for name, input := range map[string][]byte{
		"stdlib":     basePNG(t),
		"palette":    palettePNG(),
		"interlaced": interlacedPNG(),
	} {
		fields, err := Handler{}.Inspect(bytes.NewReader(input))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(fields) != 0 {
			t.Errorf("%s: clean PNG reported metadata: %v", name, fields)
		}
	}
}

func TestStrip(t *testing.T) {
	in := metadataPNG(t)
	out := strip(t, in)
	assertCleanCopy(t, in, out)
	for _, secret := range []string{"Jane", "Pixel", "SECRET", "private", "eXIf", "tIME"} {
		if bytes.Contains(out, []byte(secret)) {
			t.Errorf("output still contains %q", secret)
		}
	}
}

func TestStripImageKinds(t *testing.T) {
	tests := map[string][]byte{
		"stdlib":     basePNG(t),
		"palette":    palettePNG(),
		"interlaced": interlacedPNG(),
		"padding":    paddingPNG(),
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			assertCleanCopy(t, in, strip(t, in))
		})
	}
}

func TestStripKeepsRenderingChunks(t *testing.T) {
	gama := encodeChunk("gAMA", []byte{0, 0, 0xb1, 0x8f})
	chrm := encodeChunk("cHRM", srgbChromaticities())
	text := encodeChunk("tEXt", []byte("Software\x00Editor 1.0"))
	ihdr, idat := plainChunks()
	in := pngOf(ihdr, gama, text, chrm, idat, iendChunk)

	out := strip(t, in)
	if got, want := chunkTypes(out), []string{"IHDR", "gAMA", "cHRM", "IDAT", "IEND"}; !slices.Equal(got, want) {
		t.Errorf("chunks %v, want %v", got, want)
	}
	if !bytes.Contains(out, gama) || !bytes.Contains(out, chrm) {
		t.Error("rendering chunks changed")
	}
	assertCleanCopy(t, in, out)

	pal := strip(t, palettePNG())
	if got, want := chunkTypes(pal), []string{"IHDR", "PLTE", "tRNS", "bKGD", "IDAT", "IEND"}; !slices.Equal(got, want) {
		t.Errorf("palette chunks %v, want %v", got, want)
	}

	// mDCV and cLLI are used together with cICP.
	hdr := pngOf(ihdr, encodeChunk("cICP", []byte{9, 16, 0, 1}), encodeChunk("mDCV", validMDCV()),
		encodeChunk("cLLI", []byte{0, 0x0f, 0x42, 0x40, 0, 0x03, 0x0d, 0x40}), idat, iendChunk)
	if got, want := chunkTypes(strip(t, hdr)), []string{"IHDR", "cICP", "mDCV", "cLLI", "IDAT", "IEND"}; !slices.Equal(got, want) {
		t.Errorf("HDR chunks %v, want %v", got, want)
	}

	// A transparent gray value that fits a 4-bit sample.
	gray := pngOf(ihdrChunk(4, 1, 4, colorGray, 0), encodeChunk("tRNS", []byte{0, 15}),
		encodeChunk("IDAT", deflate([]byte{0, 0x0f, 0x37})), iendChunk)
	if got, want := chunkTypes(strip(t, gray)), []string{"IHDR", "tRNS", "IDAT", "IEND"}; !slices.Equal(got, want) {
		t.Errorf("gray chunks %v, want %v", got, want)
	}
}

// srgbChromaticities returns cHRM data for the sRGB white point and primaries.
func srgbChromaticities() []byte {
	var b []byte
	for _, v := range []uint32{31270, 32900, 64000, 33000, 30000, 60000, 15000, 6000} {
		b = binary.BigEndian.AppendUint32(b, v)
	}
	return b
}

// validMDCV returns mDCV data for a Display P3 mastering display.
func validMDCV() []byte {
	var b []byte
	for _, v := range []uint16{34000, 16000, 13250, 34500, 7500, 3000, 15635, 16450} {
		b = binary.BigEndian.AppendUint16(b, v)
	}
	b = binary.BigEndian.AppendUint32(b, 10_000_000)
	return binary.BigEndian.AppendUint32(b, 1)
}

func TestCheckValues(t *testing.T) {
	l := layout{hdr: header{colorType: colorRGB, depth: 8}}
	u32 := func(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
	chrm := func(i int, v uint32) []byte {
		b := srgbChromaticities()
		binary.BigEndian.PutUint32(b[4*i:], v)
		return b
	}
	tests := []struct {
		typ   string
		data  []byte
		valid bool
	}{
		{"gAMA", u32(15), false},
		{"gAMA", u32(16), true},
		{"gAMA", u32(45455), true},
		{"gAMA", u32(625000000), true},
		{"gAMA", u32(625000001), false},
		{"cHRM", srgbChromaticities(), true},
		{"cHRM", make([]byte, 32), false}, // white point y = 0
		{"cHRM", chrm(1, 4), false},       // white point y too small
		{"cHRM", chrm(2, 100001), false},  // red x > 1
		{"cHRM", chrm(3, 70000), false},   // red x + y > 1
		{"cHRM", chrm(3, 0), true},        // wide gamut primaries may sit on an axis
		{"mDCV", validMDCV(), true},
		{"mDCV", append([]byte{0xc3, 0x51}, validMDCV()[2:]...), false}, // x = 50001
		{"cICP", []byte{1, 13, 0, 1}, true},
		{"cICP", []byte{1, 13, 1, 1}, false},
		{"cICP", []byte{1, 13, 0, 2}, false},
	}
	for _, tt := range tests {
		if got := l.checkValues(tt.typ, tt.data) == ""; got != tt.valid {
			t.Errorf("%s %x: valid = %v, want %v", tt.typ, tt.data, got, tt.valid)
		}
	}
}

// Each case hides data where a decoder never looks. Strip must remove it,
// Inspect must report it and Verify must reject the input.
func TestHiddenData(t *testing.T) {
	const secret = "SECRET owner=alice@example.com"
	ihdr, idat := plainChunks()
	stream := testStream()
	gama := encodeChunk("gAMA", []byte{0, 0, 0xb1, 0x8f})
	palIHDR := ihdrChunk(4, 1, 8, colorPalette, 0)
	plte := encodeChunk("PLTE", []byte{9, 9, 9, 1, 2, 3})
	palIDAT := encodeChunk("IDAT", deflate([]byte{0, 0, 1, 0, 1}))

	tests := []struct {
		name  string
		input []byte
		field string
		ref   []byte // image to compare pixels with, if the input doesn't decode
	}{
		{"payload in gAMA",
			pngOf(ihdr, encodeChunk("gAMA", append([]byte{0, 0, 0xb1, 0x8f}, secret...)), idat, iendChunk),
			"gAMA chunk", nil},
		{"payload in pHYs",
			pngOf(ihdr, encodeChunk("pHYs", append(make([]byte, 9), secret...)), idat, iendChunk),
			"pHYs chunk", nil},
		{"duplicate gAMA",
			pngOf(ihdr, gama, encodeChunk("gAMA", []byte(secret[:4])), idat, iendChunk),
			"gAMA chunk", nil},
		{"gAMA after image data",
			pngOf(ihdr, idat, encodeChunk("gAMA", []byte(secret[:4])), iendChunk),
			"gAMA chunk", nil},
		{"suggested palette in RGB image",
			pngOf(ihdr, encodeChunk("PLTE", []byte(secret[:27])), idat, iendChunk),
			"Suggested palette (PLTE)", nil},
		{"data after zlib stream in IDAT",
			pngOf(ihdr, encodeChunk("IDAT", append(deflate(stream), secret...)), iendChunk),
			"Hidden data", nil},
		{"extra IDAT after zlib stream",
			pngOf(ihdr, idat, encodeChunk("IDAT", []byte(secret)), iendChunk),
			"Hidden data", nil},
		{"extra decompressed data",
			pngOf(ihdr, encodeChunk("IDAT", deflate(append(stream, secret...))), iendChunk),
			"Hidden data", pngOf(ihdr, idat, iendChunk)},
		{"fdAT without acTL",
			pngOf(ihdr, idat, encodeChunk("fdAT", append([]byte{0, 0, 0, 1}, secret...)), iendChunk),
			"fdAT chunk", nil},
		{"unused bits",
			paddingPNG(),
			"Hidden data", nil},
		{"out-of-range palette indexes",
			pngOf(ihdrChunk(4, 1, 8, colorPalette, 0), encodeChunk("PLTE", []byte{9, 9, 9}),
				encodeChunk("IDAT", deflate([]byte("\x00"+secret[:4]))), iendChunk),
			"Hidden data", nil},
		{"invalid sRGB intent",
			pngOf(ihdr, encodeChunk("sRGB", []byte{0xee}), idat, iendChunk),
			"sRGB chunk", nil},
		{"zero gamma",
			pngOf(ihdr, encodeChunk("gAMA", make([]byte, 4)), idat, iendChunk),
			"gAMA chunk", nil},
		{"gamma out of range",
			pngOf(ihdr, encodeChunk("gAMA", []byte(secret[:4])), idat, iendChunk),
			"gAMA chunk", nil},
		{"invalid chromaticities",
			pngOf(ihdr, encodeChunk("cHRM", []byte(secret[:30]+"..")), idat, iendChunk),
			"cHRM chunk", nil},
		{"gAMA overridden by sRGB",
			pngOf(ihdr, gama, encodeChunk("sRGB", []byte{0}), idat, iendChunk),
			"gAMA chunk", nil},
		{"sRGB overridden by cICP",
			pngOf(ihdr, encodeChunk("sRGB", []byte{0}), encodeChunk("cICP", []byte{1, 13, 0, 1}), idat, iendChunk),
			"sRGB chunk", nil},
		{"mDCV without cICP",
			pngOf(ihdr, encodeChunk("mDCV", validMDCV()), idat, iendChunk),
			"mDCV chunk", nil},
		{"cLLI without cICP",
			pngOf(ihdr, encodeChunk("cLLI", []byte{0, 0x0f, 0x42, 0x40, 0, 0x03, 0x0d, 0x40}), idat, iendChunk),
			"cLLI chunk", nil},
		{"invalid mastering display",
			pngOf(ihdr, encodeChunk("cICP", []byte{9, 16, 0, 1}), encodeChunk("mDCV", []byte(secret[:24])), idat, iendChunk),
			"mDCV chunk", nil},
		{"invalid cICP matrix",
			pngOf(ihdr, encodeChunk("cICP", []byte{1, 13, 1, 1}), idat, iendChunk),
			"cICP chunk", nil},
		{"gAMA after PLTE",
			pngOf(palIHDR, plte, gama, palIDAT, iendChunk),
			"gAMA chunk", nil},
		{"palette background out of range",
			pngOf(palIHDR, plte, encodeChunk("bKGD", []byte{2}), palIDAT, iendChunk),
			"bKGD chunk", nil},
		{"tRNS longer than PLTE",
			pngOf(palIHDR, plte, encodeChunk("tRNS", []byte{1, 2, 3}), palIDAT, iendChunk),
			"tRNS chunk", pngOf(palIHDR, plte, palIDAT, iendChunk)},
		{"tRNS before PLTE",
			pngOf(palIHDR, encodeChunk("tRNS", []byte{1}), plte, palIDAT, iendChunk),
			"tRNS chunk", pngOf(palIHDR, plte, palIDAT, iendChunk)},
		{"gray transparency out of range",
			pngOf(ihdrChunk(4, 1, 4, colorGray, 0), encodeChunk("tRNS", []byte{0, 16}),
				encodeChunk("IDAT", deflate([]byte{0, 0x0f, 0x37})), iendChunk),
			"tRNS chunk", nil},
		{"background sample out of range",
			pngOf(ihdr, encodeChunk("bKGD", []byte("SECRET")), idat, iendChunk),
			"bKGD chunk", nil},
		{"sBIT",
			pngOf(ihdr, encodeChunk("sBIT", []byte{8, 8, 8}), idat, iendChunk),
			"sBIT chunk", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h Handler
			fields, err := h.Inspect(bytes.NewReader(tt.input))
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if _, ok := fieldMap(fields)[tt.field]; !ok {
				t.Errorf("Inspect = %v, want a %q field", fields, tt.field)
			}
			if err := h.Verify(bytes.NewReader(tt.input)); !errors.Is(err, format.ErrMetadataRemains) {
				t.Errorf("Verify = %v, want ErrMetadataRemains", err)
			}
			out := strip(t, tt.input)
			if bytes.Contains(out, []byte(secret[:4])) {
				t.Error("secret survived Strip")
			}
			ref := tt.input
			if tt.ref != nil {
				ref = tt.ref
			}
			assertCleanCopy(t, ref, out)
		})
	}
}

func TestVerifyFindsMetadata(t *testing.T) {
	tests := map[string][]byte{
		"text chunk":    withChunks(basePNG(t), encodeChunk("tEXt", []byte("a\x00b"))),
		"ICC profile":   withChunks(basePNG(t), encodeChunk("iCCP", []byte("p\x00\x00"))),
		"trailing data": append(basePNG(t), "x"...),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			err := Handler{}.Verify(bytes.NewReader(input))
			if !errors.Is(err, format.ErrMetadataRemains) {
				t.Errorf("Verify = %v, want ErrMetadataRemains", err)
			}
		})
	}
}

func TestRejectsInvalidInput(t *testing.T) {
	base := basePNG(t)
	badCRC := bytes.Clone(base)
	badCRC[ihdrEnd-1] ^= 0xff
	hugeLen := withChunks(base, []byte("\x80\x00\x00\x00tEXt"))
	ihdr, idat := plainChunks()
	stream := testStream()
	badFilter := bytes.Clone(stream)
	badFilter[0] = 5

	tests := []struct {
		name       string
		input      []byte
		want       error
		wantVerify error // if Verify stops earlier with a different error
	}{
		{"empty", nil, format.ErrMalformed, nil},
		{"bad signature", append([]byte("\x89PNG\r\n\x1a\x00"), base[8:]...), format.ErrMalformed, nil},
		{"truncated", base[:len(base)-20], format.ErrMalformed, nil},
		{"missing IEND", base[:len(base)-len(iendChunk)], format.ErrMalformed, nil},
		{"bad CRC", badCRC, format.ErrMalformed, nil},
		{"length over limit", hugeLen, format.ErrMalformed, nil},
		{"invalid chunk type", withChunks(base, encodeChunk("tE1t", nil)), format.ErrMalformed, nil},
		{"IEND with data", append(base[:len(base)-len(iendChunk)], encodeChunk("IEND", []byte("x"))...), format.ErrMalformed, nil},
		{"first chunk not IHDR", append([]byte(signature), base[ihdrEnd:]...), format.ErrMalformed, nil},
		{"unknown critical chunk", withChunks(base, encodeChunk("ABCD", nil)), format.ErrUnsupported, nil},
		{"APNG", pngOf(ihdr, encodeChunk("acTL", make([]byte, 8)), idat, iendChunk), format.ErrUnsupported, nil},
		{"image too wide", pngOf(ihdrChunk(1<<22, 1, 8, colorRGBA, 0), idat, iendChunk), format.ErrUnsupported, nil},
		{"image too large", pngOf(ihdrChunk(1, 400_000_000, 8, colorGray, 0), idat, iendChunk), format.ErrUnsupported, nil},
		{"invalid bit depth", pngOf(ihdrChunk(4, 4, 3, colorRGB, 0), idat, iendChunk), format.ErrMalformed, nil},
		{"zero width", pngOf(ihdrChunk(0, 4, 8, colorRGB, 0), idat, iendChunk), format.ErrMalformed, nil},
		{"no image data", pngOf(ihdr, iendChunk), format.ErrMalformed, nil},
		{"too little image data", pngOf(ihdr, encodeChunk("IDAT", deflate(stream[:20])), iendChunk), format.ErrMalformed, nil},
		{"invalid filter type", pngOf(ihdr, encodeChunk("IDAT", deflate(badFilter)), iendChunk), format.ErrMalformed, nil},
		{"corrupt zlib", pngOf(ihdr, encodeChunk("IDAT", []byte("not zlib at all")), iendChunk), format.ErrMalformed, nil},
		{"IDAT not consecutive", pngOf(ihdr, idat, encodeChunk("tEXt", []byte("a\x00b")), idat, iendChunk), format.ErrMalformed, format.ErrMetadataRemains},
		{"palette image without PLTE", pngOf(ihdrChunk(4, 4, 8, colorPalette, 0), idat, iendChunk), format.ErrMalformed, nil},
		{"PLTE in grayscale", pngOf(ihdrChunk(4, 4, 8, colorGray, 0), encodeChunk("PLTE", make([]byte, 3)), idat, iendChunk), format.ErrMalformed, nil},
		{"duplicate IHDR", pngOf(ihdr, ihdr, idat, iendChunk), format.ErrMalformed, nil},
		{"PLTE too long for bit depth", pngOf(ihdrChunk(4, 1, 1, colorPalette, 0), encodeChunk("PLTE", make([]byte, 9)),
			encodeChunk("IDAT", deflate([]byte{0, 0})), iendChunk), format.ErrMalformed, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h Handler
			if _, err := h.Inspect(bytes.NewReader(tt.input)); !errors.Is(err, tt.want) {
				t.Errorf("Inspect = %v, want %v", err, tt.want)
			}
			if err := h.Strip(bytes.NewReader(tt.input), &bytes.Buffer{}); !errors.Is(err, tt.want) {
				t.Errorf("Strip = %v, want %v", err, tt.want)
			}
			wantVerify := tt.want
			if tt.wantVerify != nil {
				wantVerify = tt.wantVerify
			}
			if err := h.Verify(bytes.NewReader(tt.input)); !errors.Is(err, wantVerify) {
				t.Errorf("Verify = %v, want %v", err, wantVerify)
			}
		})
	}
}

// failingReader returns err once n bytes have been read.
type failingReader struct {
	r   io.Reader
	n   int
	err error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, f.err
	}
	n, err := f.r.Read(p[:min(len(p), f.n)])
	f.n -= n
	return n, err
}

// A read error is not a malformed file: it must reach the caller unchanged,
// wherever it happens.
func TestReadErrors(t *testing.T) {
	in := metadataPNG(t)
	for n := range len(in) {
		var h Handler
		err := h.Verify(&failingReader{bytes.NewReader(in), n, context.Canceled})
		if !errors.Is(err, context.Canceled) && !errors.Is(err, format.ErrMetadataRemains) {
			t.Fatalf("error after %d bytes: Verify = %v, want context.Canceled", n, err)
		}
		err = h.Strip(&failingReader{bytes.NewReader(in), n, context.Canceled}, io.Discard)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error after %d bytes: Strip = %v, want context.Canceled", n, err)
		}
	}
}

// A small zTXt chunk must not expand into a huge value during Inspect.
func TestInspectLimitsDecompression(t *testing.T) {
	bomb := join([]byte("Comment\x00\x00"), deflate(make([]byte, 50<<20)))
	fields, err := Handler{}.Inspect(bytes.NewReader(withChunks(basePNG(t), encodeChunk("zTXt", bomb))))
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 {
		t.Fatalf("got %d fields, want 1", len(fields))
	}
	if n := len(fields[0].Value); n > maxValueLen+64 {
		t.Errorf("value is %d bytes, want at most about %d", n, maxValueLen)
	}
}

// Many chunks must not produce an unbounded number of fields.
func TestInspectLimitsFields(t *testing.T) {
	var many []byte
	for range 5000 {
		many = append(many, encodeChunk("abCd", nil)...)
	}
	for i := range 2 * maxFields {
		key := fmt.Sprintf("key%d\x00value", i)
		many = append(many, encodeChunk("tEXt", []byte(key))...)
	}
	fields, err := Handler{}.Inspect(bytes.NewReader(withChunks(basePNG(t), many)))
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != maxFields+1 {
		t.Errorf("got %d fields, want %d", len(fields), maxFields+1)
	}
	got := fieldMap(fields)
	if got["abCd chunk"] != "5000 chunks, 0 bytes" {
		t.Errorf("abCd chunk = %q, want grouped count", got["abCd chunk"])
	}
	if got["More metadata"] != fmt.Sprintf("%d further items not shown", maxFields+1) {
		t.Errorf("More metadata = %q", got["More metadata"])
	}
}

func TestInspectClipsNames(t *testing.T) {
	key := strings.Repeat("\xe9", 100000)
	in := withChunks(basePNG(t), encodeChunk("tEXt", []byte(key+"\x00value")))
	fields, err := Handler{}.Inspect(bytes.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(fields[0].Name); n > maxNameLen+64 {
		t.Errorf("name is %d bytes, want at most about %d", n, maxNameLen)
	}
}

// TestFixture keeps testdata/metadata.png, a file with known metadata used by
// the fuzz corpus and the leak checker. Regenerate it with -update.
func TestFixture(t *testing.T) {
	path := filepath.Join("testdata", "metadata.png")
	if *update {
		if err := os.WriteFile(path, metadataPNG(t), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run TestFixture -update)", err)
	}
	if !bytes.Equal(data, metadataPNG(t)) {
		t.Error("fixture is out of date (run go test -run TestFixture -update)")
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
