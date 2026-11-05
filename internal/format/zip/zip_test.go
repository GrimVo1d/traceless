package zip

import (
	stdzip "archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"image"
	"image/color"
	stdpng "image/png"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"traceless/internal/format"
	"traceless/internal/format/jpeg"
	"traceless/internal/format/png"
)

var update = flag.Bool("update", false, "rewrite testdata fixtures")

func fieldMap(fields []format.Field) map[string]string {
	m := make(map[string]string, len(fields))
	for _, f := range fields {
		m[f.Name] = f.Value
	}
	return m
}

func inspect(t *testing.T, b []byte) map[string]string {
	t.Helper()
	fields, err := handler().Inspect(budget(), bytes.NewReader(b))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	return fieldMap(fields)
}

// cleanPNG is a stripped image, which a stripped archive keeps as it is.
func cleanPNG(t *testing.T) []byte { return stripWith(t, png.Handler{}, metadataPNG(t)) }

func TestMatch(t *testing.T) {
	for _, tt := range []struct {
		header string
		want   bool
	}{
		{"PK\x03\x04rest", true},
		{"PK\x05\x06rest", true},
		{"PK\x07\x08", false}, // a split archive's marker
		{"PK\x01\x02", false},
		{"\x89PNG", false},
		{"", false},
	} {
		if got := (Handler{}).Match([]byte(tt.header)); got != tt.want {
			t.Errorf("Match(%q) = %v", tt.header, got)
		}
	}
}

func TestStrip(t *testing.T) {
	in := metadataZIP(t)
	out := stripWith(t, handler(), in)
	for _, s := range secrets {
		if bytes.Contains(out, []byte(s)) {
			t.Errorf("stripped archive still holds %q", s)
		}
	}
	got := entries(t, out)
	want := map[string][]byte{
		"photo.png":       stripWith(t, png.Handler{}, metadataPNG(t)),
		"photos/":         {},
		"photos/shot.jpg": stripWith(t, jpeg.Handler{}, metadataJPEG(t)),
		"run.sh.png":      stripWith(t, png.Handler{}, metadataPNG(t)),
		"empty":           {},
	}
	if len(got) != len(want) {
		t.Errorf("entries %v, want %v", slices.Sorted(mapsKeys(got)), slices.Sorted(mapsKeys(want)))
	}
	for name, data := range want {
		if !bytes.Equal(got[name], data) {
			t.Errorf("%s holds %d bytes, want its stripped %d bytes", name, len(got[name]), len(data))
		}
	}

	zr, err := stdzip.NewReader(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatal(err)
	}
	if zr.Comment != "" {
		t.Errorf("archive comment %q", zr.Comment)
	}
	modes := map[string]os.FileMode{"photo.png": 0o644, "photos/": os.ModeDir | 0o755, "run.sh.png": 0o755}
	for _, f := range zr.File {
		if want, ok := modes[f.Name]; ok && f.Mode() != want {
			t.Errorf("%s mode %v, want %v", f.Name, f.Mode(), want)
		}
		if !f.Modified.Equal(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)) || len(f.Extra) > 0 || f.Comment != "" {
			t.Errorf("%s keeps a date (%v), extra field (%x) or comment (%q)", f.Name, f.Modified, f.Extra, f.Comment)
		}
	}
	if err := handler().Verify(budget(), bytes.NewReader(out)); err != nil {
		t.Errorf("Verify of the stripped archive: %v", err)
	}
	if fields := inspect(t, out); len(fields) != 0 {
		t.Errorf("Inspect of the stripped archive: %v", fields)
	}
	if again := stripWith(t, handler(), out); !bytes.Equal(again, out) {
		t.Error("Strip is not idempotent")
	}
}

func mapsKeys(m map[string][]byte) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func TestInspect(t *testing.T) {
	got := inspect(t, metadataZIP(t))
	for name, value := range map[string]string{
		"Archive comment":                           "archive comment secret",
		"Comment on photo.png":                      "entry comment secret",
		"Entry dates":                               "5 entries, 2024-05-06 07:08:10",
		"Extended timestamps (extra field)":         "5 entries, 25 bytes",
		"Unix owner (extra field): uid 501, gid 20": "photo.png",
		"Permissions 0600":                          "photo.png",
		"System metadata files":                     "2 entries, 42 bytes",
		"photo.png: tEXt Author":                    "Jane Doe",
		"photos/shot.jpg: IPTC City":                "Berlin",
	} {
		if got[name] != value {
			t.Errorf("%s = %q, want %q", name, got[name], value)
		}
	}
	if _, ok := got["Permissions 0755"]; ok {
		t.Error("an executable file's mode is reported")
	}
}

// Each kind of metadata is reported by Inspect and fails Verify.
func TestMetadata(t *testing.T) {
	img := cleanPNG(t)
	for _, tt := range []struct {
		name  string
		zip   []byte
		field string
	}{
		{"date", rawZIP("", rawEntry{name: "a.png", data: img, date: 0x5821, time: 0x6000}), "Entry dates"},
		{"central directory date", rawZIP("", rawEntry{name: "a.png", data: img, cdate: 0x5821}), "Entry dates"},
		{"extended timestamp", rawZIP("", rawEntry{name: "a.png", data: img, extra: []byte{0x55, 0x54, 5, 0, 1, 1, 2, 3, 4}}), "Extended timestamps (extra field)"},
		{"NTFS times", rawZIP("", rawEntry{name: "a.png", data: img, cextra: []byte{0x0a, 0, 4, 0, 0, 0, 0, 0}}), "NTFS timestamps (extra field)"},
		{"Unix owner", rawZIP("", rawEntry{name: "a.png", data: img, extra: []byte{0x55, 0x78, 4, 0, 0xe8, 3, 0x64, 0}}), "Unix owner (extra field): uid 1000, gid 100"},
		{"unknown extra field", rawZIP("", rawEntry{name: "a.png", data: img, extra: []byte{0xfe, 0xca, 0, 0}}), "Extra field 0xcafe"},
		{"bad extra field", rawZIP("", rawEntry{name: "a.png", data: img, extra: []byte{1, 2, 9}}), "Extra field data"},
		{"entry comment", rawZIP("", rawEntry{name: "a.png", data: img, comment: []byte("hi")}), "Comment on a.png"},
		{"archive comment", rawZIP("hi", rawEntry{name: "a.png", data: img}), "Archive comment"},
		{"permissions", rawZIP("", rawEntry{name: "a.png", data: img, external: 0o100600 << 16}), "Permissions 0600"},
		{"archiver", rawZIP("", rawEntry{name: "a.png", data: img, madeBy: 0x0314 + 10}), "Archiver: Unix, ZIP 3.0"},
		{"DOS attributes", rawZIP("", rawEntry{name: "a.png", data: img, madeBy: 0x0014, external: 0x21}), "DOS attributes 0x21"},
		{"system file", rawZIP("", rawEntry{name: "a.png", data: img}, rawEntry{name: "Thumbs.db", data: []byte("x")}), "System metadata files"},
		{"hidden data", rawZIP("", rawEntry{name: "a.png", data: img, hidden: []byte("secret")}), "Hidden data"},
		{"image metadata", rawZIP("", rawEntry{name: "a.png", data: metadataPNG(t)}), "a.png: tEXt Author"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := inspect(t, tt.zip)[tt.field]; !ok {
				t.Errorf("Inspect doesn't report %q: %v", tt.field, inspect(t, tt.zip))
			}
			if err := handler().Verify(budget(), bytes.NewReader(tt.zip)); !errors.Is(err, format.ErrMetadataRemains) {
				t.Errorf("Verify = %v, want ErrMetadataRemains", err)
			}
			out := stripWith(t, handler(), tt.zip)
			if fields := inspect(t, out); len(fields) != 0 {
				t.Errorf("Inspect of the stripped archive: %v", fields)
			}
			if err := handler().Verify(budget(), bytes.NewReader(out)); err != nil {
				t.Errorf("Verify of the stripped archive: %v", err)
			}
		})
	}
}

// Raw archives without metadata still differ from what Strip writes: dates,
// data descriptors, compression.
func TestVerifyNotStripped(t *testing.T) {
	raw := rawZIP("", rawEntry{name: "a.png", data: cleanPNG(t), date: dosDate})
	if fields := inspect(t, raw); len(fields) != 0 {
		t.Errorf("Inspect: %v", fields)
	}
	if err := handler().Verify(budget(), bytes.NewReader(raw)); !errors.Is(err, errNotStripped) {
		t.Errorf("Verify = %v, want errNotStripped", err)
	}
}

func TestEmptyArchive(t *testing.T) {
	empty := rawZIP("")
	if len(empty) != 22 {
		t.Fatalf("empty archive of %d bytes", len(empty))
	}
	if out := stripWith(t, handler(), empty); !bytes.Equal(out, empty) {
		t.Errorf("Strip of an empty archive = %x", out)
	}
	if err := handler().Verify(budget(), bytes.NewReader(empty)); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// An archive inside an archive is cleaned like any entry.
func TestNested(t *testing.T) {
	outer := rawZIP("", rawEntry{name: "inner.zip", data: metadataZIP(t)})
	if got := inspect(t, outer)["inner.zip: photo.png: tEXt Author"]; got != "Jane Doe" {
		t.Errorf("nested PNG field = %q", got)
	}
	out := stripWith(t, handler(), outer)
	if got, want := entries(t, out)["inner.zip"], stripWith(t, handler(), metadataZIP(t)); !bytes.Equal(got, want) {
		t.Error("the inner archive isn't stripped")
	}
	if err := handler().Verify(budget(), bytes.NewReader(out)); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// Verify checks a nested file with its own Verify, so it streams where a
// nested Strip holds its output back: a palette PNG holds its pixels until
// the last one, further than the fixed-point check can lag (4 MiB).
func TestVerifyLargePalettePNG(t *testing.T) {
	pal := make(color.Palette, 256)
	for i := range pal {
		pal[i] = color.RGBA{uint8(i), uint8(255 - i), uint8(i / 2), 255}
	}
	img := image.NewPaletted(image.Rect(0, 0, 2400, 2400), pal)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.UintN(256))
	}
	var buf bytes.Buffer
	if err := stdpng.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	clean := stripWith(t, png.Handler{}, buf.Bytes())
	if len(clean) < 5<<20 {
		t.Fatalf("image of %d bytes, want over 5 MB", len(clean))
	}
	out := stripWith(t, handler(), rawZIP("", rawEntry{name: "a.png", data: clean}))
	if err := handler().Verify(budget(), bytes.NewReader(out)); err != nil {
		t.Errorf("Verify of the stripped archive: %v", err)
	}
}

// Names that only look alike to some extractors are fine as long as they
// can't be the same file.
func TestNames(t *testing.T) {
	img := cleanPNG(t)
	for name, z := range map[string][]byte{
		"folder after its files":  rawZIP("", rawEntry{name: "d/a.png", data: img}, rawEntry{name: "d/", stored: true, external: 0o40755 << 16}),
		"folder before its files": rawZIP("", rawEntry{name: "d/", stored: true, external: 0o40755 << 16}, rawEntry{name: "d/a.png", data: img}),
		"folders in other cases":  rawZIP("", rawEntry{name: "D/a.png", data: img}, rawEntry{name: "d/b.png", data: img}),
		"dots inside names":       rawZIP("", rawEntry{name: "a..b/.x.png", data: img}),
		"UTF-8":                   rawZIP("", rawEntry{name: "фото/café.png", data: img, flags: flagUTF8}),
		"device-like names":       rawZIP("", rawEntry{name: "console/com10.png", data: img}),
	} {
		t.Run(name, func(t *testing.T) {
			out := stripWith(t, handler(), z)
			if err := handler().Verify(budget(), bytes.NewReader(out)); err != nil {
				t.Errorf("Verify of the stripped archive: %v", err)
			}
		})
	}
}

func TestDepth(t *testing.T) {
	z := cleanZIP(t)
	for range 5 {
		z = rawZIP("", rawEntry{name: "a.zip", data: z})
	}
	if err := handler().Strip(budget(), bytes.NewReader(z), io.Discard); !errors.Is(err, format.ErrLimit) {
		t.Errorf("Strip of archives nested 6 deep = %v, want ErrLimit", err)
	}
}

// An entry of a format no handler cleans is reported by Inspect and makes
// Strip and Verify refuse the archive.
func TestUnknownEntry(t *testing.T) {
	z := rawZIP("", rawEntry{name: "a.png", data: cleanPNG(t)}, rawEntry{name: "notes.txt", data: []byte("hello")})
	if got := inspect(t, z)["Entries that can't be cleaned"]; got != "notes.txt" {
		t.Errorf("Inspect reports %q", got)
	}
	if err := handler().Strip(budget(), bytes.NewReader(z), io.Discard); !errors.Is(err, format.ErrUnsupported) {
		t.Errorf("Strip = %v, want ErrUnsupported", err)
	}
	if err := handler().Verify(budget(), bytes.NewReader(stripWith(t, handler(), rawZIP("", rawEntry{name: "a.png", data: cleanPNG(t)})))); err != nil {
		t.Errorf("Verify of a clean archive: %v", err)
	}
	// Without nested handlers, every file is unknown.
	if err := (Handler{}).Strip(budget(), bytes.NewReader(cleanZIP(t)), io.Discard); !errors.Is(err, format.ErrUnsupported) {
		t.Errorf("Strip without handlers = %v, want ErrUnsupported", err)
	}
}

// eocd returns the offset of the end record of a rawZIP archive without a
// comment.
func eocd(b []byte) int { return len(b) - 22 }

func put16(b []byte, off int, v uint16) []byte {
	b = bytes.Clone(b)
	binary.LittleEndian.PutUint16(b[off:], v)
	return b
}

func put32(b []byte, off int, v uint32) []byte {
	b = bytes.Clone(b)
	binary.LittleEndian.PutUint32(b[off:], v)
	return b
}

// check runs all three methods on input: Inspect and Strip must fail with
// want, Verify with want or ErrMetadataRemains, since it stops at the first
// byte that differs from the rebuilt archive.
func check(t *testing.T, input []byte, want error) {
	t.Helper()
	h := handler()
	if _, err := h.Inspect(budget(), bytes.NewReader(input)); !errors.Is(err, want) {
		t.Errorf("Inspect = %v, want %v", err, want)
	}
	if err := h.Strip(budget(), bytes.NewReader(input), io.Discard); !errors.Is(err, want) {
		t.Errorf("Strip = %v, want %v", err, want)
	}
	if err := h.Verify(budget(), bytes.NewReader(input)); !errors.Is(err, want) && !errors.Is(err, format.ErrMetadataRemains) {
		t.Errorf("Verify = %v, want %v", err, want)
	}
}

func TestMalformed(t *testing.T) {
	img := cleanPNG(t)
	base := rawZIP("", rawEntry{name: "a.png", data: img, stored: true})
	two := rawZIP("", rawEntry{name: "a.png", data: img}, rawEntry{name: "b.png", data: img})
	cd := int(binary.LittleEndian.Uint32(base[eocd(base)+16:]))
	for _, tt := range []struct {
		name  string
		input []byte
	}{
		{"truncated header", base[:20]},
		{"truncated data", base[:60]},
		{"no end record", base[:eocd(base)]},
		{"truncated end record", base[:len(base)-1]},
		{"CRC mismatch", put32(base, 14, 1)},
		{"stored sizes differ", put32(base, 22, uint32(len(img)+1))},
		{"corrupt deflate stream", rawZIP("", rawEntry{name: "a.png", data: img, comp: []byte{0xff, 0xff, 0xff}})},
		{"trailing data", append(bytes.Clone(base), 'x')},
		{"gap between entries", rawZIP("", rawEntry{name: "a.png", data: img, gap: []byte("xxxx")}, rawEntry{name: "b.png", data: img})},
		{"missing central directory", append(base[:cd:cd], rawZIP("")...)},
		{"central name differs", rawZIP("", rawEntry{name: "a.png", data: img, cname: "b.png"})},
		{"central offset differs", put32(two, eocd(two)-46-5+42, 1)},
		{"central CRC differs", put32(base, cd+16, 1)},
		{"end record count", put16(base, eocd(base)+10, 2)},
		// The comment length 0x4b50 is "PK", and the comment goes on "\x05\x06".
		{"end record across the comment length", rawZIP("\x05\x06"+strings.Repeat("x", 0x4b50-2), rawEntry{name: "a.png", data: img})},
		{"ZIP64 locator before the end record", rawZIP("", rawEntry{name: "a.png", data: img, comment: []byte("PK\x06\x07" + strings.Repeat("x", 16))})},
		{"end record disk count", put16(base, eocd(base)+8, 2)},
		{"end record offset", put32(base, eocd(base)+16, uint32(cd+1))},
		{"duplicate names", rawZIP("", rawEntry{name: "a.png", data: img}, rawEntry{name: "a.png", data: img})},
		{"comment with an end record", rawZIP("PK\x05\x06", rawEntry{name: "a.png", data: img})},
		{"folder with data", rawZIP("", rawEntry{name: "d/", data: []byte("x"), stored: true})},
		{"folder with a file mode", rawZIP("", rawEntry{name: "d/", stored: true, external: 0o100644 << 16})},
		{"file with a folder mode", rawZIP("", rawEntry{name: "a.png", data: img, external: 0o40755 << 16})},
		{"empty name", rawZIP("", rawEntry{name: "", data: img})},
		{"invalid UTF-8 name", rawZIP("", rawEntry{name: "\xff.png", data: img, flags: flagUTF8})},
		{"broken image", rawZIP("", rawEntry{name: "a.png", data: img[:len(img)-20]})},
	} {
		t.Run(tt.name, func(t *testing.T) { check(t, tt.input, format.ErrMalformed) })
	}
}

func TestUnsupported(t *testing.T) {
	img := cleanPNG(t)
	base := rawZIP("", rawEntry{name: "a.png", data: img})
	pathExtra := func(name string) []byte {
		b := []byte{0x75, 0x70, 0, 0, 1}
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = append(b, name...)
		binary.LittleEndian.PutUint16(b[2:], uint16(len(b)-4))
		return b
	}
	for _, tt := range []struct {
		name  string
		input []byte
	}{
		{"encrypted", rawZIP("", rawEntry{name: "a.png", data: img, flags: flagEncrypted})},
		{"unknown flag", rawZIP("", rawEntry{name: "a.png", data: img, flags: 1 << 4})},
		{"bzip2", rawZIP("", rawEntry{name: "a.png", data: img, method: 12})},
		{"ZIP64 sizes", put32(base, 18, 0xffffffff)},
		{"ZIP64 extra field", rawZIP("", rawEntry{name: "a.png", data: img, extra: []byte{1, 0, 0, 0}})},
		{"ZIP64 end record", put16(base, eocd(base)+10, 0xffff)},
		{"split archive", put16(base, eocd(base)+4, 1)},
		{"stored with descriptor", rawZIP("", rawEntry{name: "a.png", data: img, stored: true, descriptor: true})},
		{"symbolic link", rawZIP("", rawEntry{name: "a.png", data: img, external: 0o120777 << 16})},
		{"absolute name", rawZIP("", rawEntry{name: "/a.png", data: img})},
		{"parent name", rawZIP("", rawEntry{name: "x/../a.png", data: img})},
		{"backslash", rawZIP("", rawEntry{name: `x\a.png`, data: img})},
		{"legacy encoding", rawZIP("", rawEntry{name: "caf\xe9.png", data: img})},
		{"drive letter", rawZIP("", rawEntry{name: "C:/a.png", data: img})},
		{"NTFS stream", rawZIP("", rawEntry{name: "a.png:ads", data: img})},
		{"device name", rawZIP("", rawEntry{name: "d/aux.png", data: img})},
		{"COM device", rawZIP("", rawEntry{name: "com\u00b9", data: img})},
		{"trailing dot", rawZIP("", rawEntry{name: "a.png.", data: img})},
		{"trailing space", rawZIP("", rawEntry{name: "d /a.png", data: img})},
		{"names differing in case", rawZIP("", rawEntry{name: "A.png", data: img}, rawEntry{name: "a.png", data: img})},
		{"file and folder", rawZIP("", rawEntry{name: "d", data: img}, rawEntry{name: "d/", stored: true, external: 0o40755 << 16})},
		{"file, then a file in it", rawZIP("", rawEntry{name: "d", data: img}, rawEntry{name: "D/a.png", data: img})},
		{"file in a folder, then the folder as a file", rawZIP("", rawEntry{name: "d/e/a.png", data: img}, rawEntry{name: "d/e", data: img})},
		{"Unicode path differs", rawZIP("", rawEntry{name: "a.png", data: img, extra: pathExtra("b.png")})},
		{"unknown format", rawZIP("", rawEntry{name: "a.txt", data: []byte("text")})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := handler()
			if err := h.Strip(budget(), bytes.NewReader(tt.input), io.Discard); !errors.Is(err, format.ErrUnsupported) {
				t.Errorf("Strip = %v, want ErrUnsupported", err)
			}
			if err := h.Verify(budget(), bytes.NewReader(tt.input)); !errors.Is(err, format.ErrUnsupported) && !errors.Is(err, format.ErrMetadataRemains) {
				t.Errorf("Verify = %v, want ErrUnsupported", err)
			}
		})
	}
}

// Names in a legacy encoding are refused, UTF-8 ones are kept with the flag.
func TestUTF8Name(t *testing.T) {
	out := stripWith(t, handler(), rawZIP("", rawEntry{name: "café.png", data: cleanPNG(t), flags: flagUTF8}))
	if _, ok := entries(t, out)["café.png"]; !ok {
		t.Errorf("entries %v", entries(t, out))
	}
}

// A small archive can't decompress to more than the budget allows.
func TestBomb(t *testing.T) {
	var big bytes.Buffer
	fw, _ := flateWriter(&big)
	zeros := make([]byte, 1<<20)
	for range 64 {
		fw.Write(zeros)
	}
	fw.Close()
	z := rawZIP("", rawEntry{name: "a.bin", comp: big.Bytes(), data: nil})
	// The CRC and sizes are wrong, but the limit stops it first.
	b := format.NewBudget(context.Background(), format.Limits{Memory: 64 << 20, Decompressed: 1 << 20, Work: 1 << 30, Depth: 4})
	if _, err := handler().Inspect(b, bytes.NewReader(z)); !errors.Is(err, format.ErrLimit) {
		t.Errorf("Inspect = %v, want ErrLimit", err)
	}
	if b.Held() != 0 {
		t.Errorf("%d bytes held", b.Held())
	}
}

// Read errors pass through as they are, so frontends can tell them from bad
// files.
func TestReadError(t *testing.T) {
	errDisk := errors.New("disk failed")
	z := cleanZIP(t)
	r := io.MultiReader(bytes.NewReader(z[:len(z)/2]), errReader{errDisk})
	if err := handler().Strip(budget(), r, io.Discard); !errors.Is(err, errDisk) {
		t.Errorf("Strip = %v, want the read error", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// TestStripGolden pins the bytes Strip writes. If a Go release changes
// compress/flate, archives stripped by an older build fail Verify; check the
// change, then run go test -run TestStripGolden -update.
func TestStripGolden(t *testing.T) {
	path := filepath.Join("testdata", "stripped.zip")
	out := stripWith(t, handler(), metadataZIP(t))
	if *update {
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run TestStripGolden -update)", err)
	}
	if !bytes.Equal(out, golden) {
		t.Error("Strip output changed (see the comment on TestStripGolden)")
	}
}

func TestFixture(t *testing.T) {
	path := filepath.Join("testdata", "metadata.zip")
	if *update {
		if err := os.WriteFile(path, metadataZIP(t), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run TestFixture -update)", err)
	}
	if !bytes.Equal(data, metadataZIP(t)) {
		t.Error("fixture is out of date (run go test -run TestFixture -update)")
	}
	if !strings.Contains(inspect(t, data)["Archive comment"], "secret") {
		t.Error("fixture has no archive comment")
	}
}
