package jpeg

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"traceless/internal/format"
)

// The files in testdata/scans were made with libjpeg-turbo 2.1 and
// ImageMagick from one 35x19 image (odd sizes, so there are padding blocks):
//
//	cjpeg -quality 85 [-grayscale | -sample 2x1] src.ppm > base.jpg
//	jpegtran -copy none -progressive [-restart 3B] base.jpg > prog.jpg
//	jpegtran -copy none -scans SCRIPT base.jpg > prog-custom.jpg
//	convert src.ppm -colorspace CMYK -strip -quality 85 [-interlace JPEG] cmyk.jpg
//
// prog-custom.jpg follows this script (component: Ss-Se, Ah, Al), which
// codes the luma DC alone, so its padding blocks are never coded:
//
//	0: 0-0, 0, 2;    1,2: 0-0, 0, 1;  0: 1-9, 0, 3;    0: 10-63, 0, 3;
//	1: 1-63, 0, 0;   2: 1-63, 0, 0;   0: 1-63, 3, 2;   0: 0-0, 2, 1;
//	1,2: 0-0, 1, 0;  0: 0-0, 1, 0;    0: 1-63, 2, 1;   0: 1-63, 1, 0;
//
// multiscan.jpg is sequential with one scan per component ("0; 1; 2;"),
// multiscan-pair.jpg with two ("0,1; 2;"). prog-many-gray.jpg has 98 scans:
// DC to bit 3, AC 1 to 48 one coefficient per scan with one refinement each,
// then AC 49-63. Its DC refinements are left to the tests.
func readScans(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "scans", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// piece is one marker segment of a file, with the entropy-coded data after
// it for SOS.
type piece struct {
	m byte
	b []byte
}

// pieces splits a well-formed file without fill bytes into segments.
func pieces(b []byte) []piece {
	ps := []piece{{markerSOI, b[:2]}}
	for i := 2; i < len(b); {
		m := b[i+1]
		if m == markerEOI {
			return append(ps, piece{m, b[i : i+2]})
		}
		end := i + 2 + int(binary.BigEndian.Uint16(b[i+2:]))
		for m == markerSOS && !(b[end] == 0xff && b[end+1] != 0 && (b[end+1] < markerRST0 || b[end+1] > markerRST7)) {
			end++
		}
		ps = append(ps, piece{m, b[i:end]})
		i = end
	}
	return ps
}

func joinPieces(ps []piece) []byte {
	var b []byte
	for _, p := range ps {
		b = append(b, p.b...)
	}
	return b
}

// scanIndexes returns the positions of the SOS pieces.
func scanIndexes(ps []piece) []int {
	var idx []int
	for i, p := range ps {
		if p.m == markerSOS {
			idx = append(idx, i)
		}
	}
	return idx
}

// Progressive and multi-scan files hold the same coefficients as their
// sequential source, so they must strip to the very same file.
func TestScans(t *testing.T) {
	tests := []struct{ in, ref string }{
		{"prog.jpg", "base.jpg"},
		{"prog-restart.jpg", "base.jpg"},
		{"prog-custom.jpg", "base.jpg"},
		{"multiscan.jpg", "base.jpg"},
		{"multiscan-pair.jpg", "base.jpg"},
		{"prog-gray.jpg", "gray.jpg"},
		{"prog422.jpg", "base422.jpg"},
		{"prog-cmyk.jpg", "cmyk.jpg"},
	}
	var h Handler
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			in, ref := readScans(t, tt.in), readScans(t, tt.ref)
			out := strip(t, in)
			if !bytes.Equal(out, strip(t, ref)) {
				t.Errorf("stripped %s differs from stripped %s", tt.in, tt.ref)
			}
			// The standard library can't decode every progressive file
			// (restart markers trip it up), so pixels are compared with
			// the sequential source; libjpeg shows both the same.
			assertCleanCopy(t, ref, out)
			if fields, err := h.Inspect(budget(), bytes.NewReader(in)); err != nil || len(fields) != 0 {
				t.Errorf("Inspect = %v, %v; want no fields", fields, err)
			}
			if err := h.Verify(budget(), bytes.NewReader(in)); !errors.Is(err, format.ErrMetadataRemains) {
				t.Errorf("Verify = %v, want ErrMetadataRemains", err)
			}
		})
	}
}

// dcRefine returns a DC refinement scan of bit al for prog-many-gray.jpg:
// 15 blocks, one zero bit each.
func dcRefine(al byte) string {
	return string(seg(markerSOS, []byte{1, 1, 0, 0, 0, (al+1)<<4 | al})) + "\x00\x01"
}

func TestScanLimit(t *testing.T) {
	many := readScans(t, "prog-many-gray.jpg")
	tests := []struct {
		name, scans, want string
	}{
		{"100 scans", dcRefine(2) + dcRefine(1), "coded in full"},
		{"101 scans", dcRefine(2) + dcRefine(1) + dcRefine(0), "more than 100 scans"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Handler{}.Inspect(budget(), bytes.NewReader(beforeEOI(many, tt.scans)))
			if !errors.Is(err, format.ErrUnsupported) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Inspect = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRejectsBadProgression(t *testing.T) {
	custom := pieces(readScans(t, "prog-custom.jpg"))
	scans := scanIndexes(custom)
	if len(scans) != 12 {
		t.Fatalf("prog-custom.jpg has %d scans, want 12", len(scans))
	}
	// edit returns prog-custom.jpg changed by fn.
	edit := func(fn func(ps []piece) []piece) []byte {
		return joinPieces(fn(slices.Clone(custom)))
	}
	// header replaces the scan header of scan i, keeping its data.
	header := func(i int, sos []byte) []byte {
		return edit(func(ps []piece) []piece {
			p := ps[scans[i]].b
			n := 2 + int(binary.BigEndian.Uint16(p[2:]))
			ps[scans[i]] = piece{markerSOS, append(seg(markerSOS, sos), p[n:]...)}
			return ps
		})
	}
	sof := func(width, height uint16) []byte {
		return edit(func(ps []piece) []piece {
			i := slices.IndexFunc(ps, func(p piece) bool { return p.m == markerSOF2 })
			b := bytes.Clone(ps[i].b)
			binary.BigEndian.PutUint16(b[5:], height)
			binary.BigEndian.PutUint16(b[7:], width)
			ps[i] = piece{markerSOF2, b}
			return ps
		})
	}

	malformedCases := map[string][]byte{
		"missing refinement": edit(func(ps []piece) []piece { return slices.Delete(ps, scans[6], scans[6]+1) }),
		"repeated scan": edit(func(ps []piece) []piece {
			return slices.Insert(ps, scans[4], ps[scans[4]])
		}),
		"AC before DC": edit(func(ps []piece) []piece {
			ac := ps[scans[2]]
			ps = slices.Delete(ps, scans[2], scans[2]+1)
			return slices.Insert(ps, scans[0], ac)
		}),
		"refinement skips a bit":  header(6, []byte{1, 1, 0x11, 1, 63, 0x31}),
		"AC with two components":  header(4, []byte{2, 2, 0x11, 3, 0x11, 1, 63, 0}),
		"DC and AC in one scan":   header(0, []byte{1, 1, 0x00, 0, 5, 0x02}),
		"bit position 14":         header(0, []byte{1, 1, 0x00, 0, 0, 0x0e}),
		"band past 63":            header(4, []byte{1, 2, 0x11, 1, 64, 0}),
		"truncated":               joinPieces(custom)[:600],
		"scan for no coefficient": header(4, []byte{1, 2, 0x11, 5, 4, 0}),
	}
	unsupportedCases := map[string][]byte{
		"last scan missing": edit(func(ps []piece) []piece { return slices.Delete(ps, scans[11], scans[11]+1) }),
		"quantization table between scans": edit(func(ps []piece) []piece {
			dqt := piece{markerDQT, seg(markerDQT, append([]byte{3}, bytes.Repeat([]byte{1}, 64)...))}
			return slices.Insert(ps, scans[3], dqt)
		}),
		"too large": sof(32767, 32767),
		"JFIF between scans": edit(func(ps []piece) []piece {
			return slices.Insert(ps, scans[3], piece{markerAPP0, app(0, jfifData(0, 0, ""))})
		}),
		"Adobe after the last scan": edit(func(ps []piece) []piece {
			return slices.Insert(ps, len(ps)-1, piece{markerAPP0 + 14, app(14, adobeData(0, ""))})
		}),
		"arithmetic": readScans(t, "prog-arith.jpg"),
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

// Tables defined between scans are fine when a later scan uses them; the
// others can carry anything.
func TestScansHiddenData(t *testing.T) {
	custom := pieces(readScans(t, "prog-custom.jpg"))
	scans := scanIndexes(custom)
	dri := piece{markerDRI, seg(markerDRI, []byte{0, 0})}
	dht := piece{markerDHT, seg(markerDHT, appendDHT(nil, 1, 3, huffSpec{[16]byte{0, 0, 0, 0, 0, 0, 6}, []byte("SECRET")}))}
	tests := []struct {
		name, value string
		in          []piece
	}{
		{"unused Huffman table", "1 unused Huffman tables", slices.Insert(slices.Clone(custom), len(custom)-1, dht)},
		{"unused restart interval", "1 unused restart intervals", slices.Insert(slices.Clone(custom), scans[3], dri, dri)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := joinPieces(tt.in)
			fields, err := Handler{}.Inspect(budget(), bytes.NewReader(in))
			if err != nil {
				t.Fatal(err)
			}
			if len(fields) != 1 || !hasField(fields, "Hidden data", tt.value) {
				t.Errorf("Inspect = %v, want Hidden data: %s", fields, tt.value)
			}
			out := strip(t, in)
			if bytes.Contains(out, []byte("SECRET")) {
				t.Error("secret survived Strip")
			}
			if !bytes.Equal(out, strip(t, readScans(t, "base.jpg"))) {
				t.Error("Strip output differs from the stripped source")
			}
		})
	}
}

// A file that declares a large progressive image but holds little data must
// not cost the whole coefficient buffer.
func TestCoefficientsGrowWithData(t *testing.T) {
	ps := pieces(readScans(t, "prog-gray.jpg"))
	i := slices.IndexFunc(ps, func(p piece) bool { return p.m == markerSOF2 })
	sof := bytes.Clone(ps[i].b)
	binary.BigEndian.PutUint16(sof[5:], 8000) // 1 million blocks: 128 MB
	binary.BigEndian.PutUint16(sof[7:], 8000)
	ps[i] = piece{markerSOF2, sof}
	in := joinPieces(ps)
	in = in[:bytes.Index(in, []byte{0xff, markerSOS})+64]

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := Handler{}.Inspect(budget(), bytes.NewReader(in))
	runtime.ReadMemStats(&after)
	if !errors.Is(err, format.ErrMalformed) {
		t.Errorf("Inspect = %v, want ErrMalformed", err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 8<<20 {
		t.Errorf("allocated %d MB for a truncated file", n>>20)
	}
}

// AC refinement in isolation, with 2-bit codes: 00 ZRL, 01 a new coefficient,
// 10 end of band.
func TestDecodeACRefine(t *testing.T) {
	d, err := newDecoder(huffSpec{[16]byte{0, 3}, []byte{0xf0, 0x01, 0x00}})
	if err != nil {
		t.Fatal(err)
	}
	reader := func(b ...byte) *bitReader { return &bitReader{r: bufio.NewReader(bytes.NewReader(b))} }

	// A ZRL needs 16 zero coefficients left in the band.
	var coef [64]int16
	var end uint8
	var eobrun int32
	err = reader(0x00).decodeACRefine(d, 1, 10, 0, &eobrun, coef[:], &end)
	if !errors.Is(err, format.ErrMalformed) {
		t.Errorf("ZRL past the band: err = %v, want malformed", err)
	}

	// In a run of blocks with nothing new, each nonzero coefficient takes one
	// correction bit; the zeros after the last one are skipped.
	coef = [64]int16{}
	coef[3], coef[20] = 2, -2
	end = 0
	setEnd(&end, 3)
	setEnd(&end, 20)
	br, eobrun := reader(0xc0), 2
	if err := br.decodeACRefine(d, 1, 63, 0, &eobrun, coef[:], &end); err != nil {
		t.Fatal(err)
	}
	if coef[3] != 3 || coef[20] != -3 || br.n != 6 || eobrun != 1 || end != 21 {
		t.Errorf("coef[3] = %d, coef[20] = %d, bits left %d, eobrun %d, end %d; want 3, -3, 6, 1, 21",
			coef[3], coef[20], br.n, eobrun, end)
	}
}
