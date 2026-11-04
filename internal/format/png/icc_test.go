package png

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"traceless/internal/format"
	"traceless/internal/format/icc"
	"traceless/internal/format/icc/icctest"
)

func iccpChunk(name string, profile []byte) []byte {
	return encodeChunk("iCCP", join([]byte(name+"\x00\x00"), deflate(profile)))
}

var (
	vendorP3 = icctest.Build(icctest.Profile{Version: 4, Colorants: icctest.DisplayP3, Curve: icctest.SRGBPara,
		Tags: map[string][]byte{"dmdd": icctest.Text("Pixel 7 Pro")}})
	vendorAdobe = icctest.Build(icctest.Profile{Version: 2, Colorants: icctest.AdobeRGB, Curve: icctest.Gamma22})
	vendorSRGB  = icctest.Build(icctest.Profile{Version: 2, Colorants: icctest.SRGB, Curve: icctest.SRGBTable})
	dciP3       = icctest.Build(icctest.Profile{Version: 4, Colorants: icctest.DCIP3, Curve: icctest.SRGBPara})
)

// Profiles of a known color space are replaced by ours, or by an sRGB
// chunk; the colors chunks they override go; other profiles are dropped.
func TestICCProfile(t *testing.T) {
	ihdr, idat := plainChunks()
	gray := ihdrChunk(4, 1, 8, colorGray, 0)
	grayIDAT := encodeChunk("IDAT", deflate([]byte{0, 1, 2, 3, 4}))
	plte := encodeChunk("PLTE", []byte{1, 2, 3})
	gama := encodeChunk("gAMA", []byte{0, 0, 0xb1, 0x8f})
	ours := func(s icc.Space) []byte { return encodeChunk("iCCP", canonicalICCP()[s]) }
	for _, tt := range []struct {
		name  string
		input []byte
		want  []byte // the stripped file
	}{
		{"Display P3", pngOf(ihdr, iccpChunk("Secret Corp camera", vendorP3), idat, iendChunk),
			pngOf(ihdr, ours(icc.DisplayP3), idat, iendChunk)},
		{"Adobe RGB", pngOf(ihdr, iccpChunk("Secret Corp camera", vendorAdobe), idat, iendChunk),
			pngOf(ihdr, ours(icc.AdobeRGB), idat, iendChunk)},
		{"sRGB becomes an sRGB chunk", pngOf(ihdr, gama, iccpChunk("Secret Corp camera", vendorSRGB), idat, iendChunk),
			pngOf(ihdr, encodeChunk("sRGB", []byte{0}), idat, iendChunk)},
		{"sRGB profile and chunk", pngOf(ihdr, encodeChunk("sRGB", []byte{2}), iccpChunk("x", vendorSRGB), idat, iendChunk),
			pngOf(ihdr, encodeChunk("sRGB", []byte{2}), idat, iendChunk)},
		{"profile overrides sRGB, gAMA, cHRM", pngOf(ihdr, encodeChunk("sRGB", []byte{0}), gama, iccpChunk("x", vendorP3), idat, iendChunk),
			pngOf(ihdr, ours(icc.DisplayP3), idat, iendChunk)},
		{"cICP overrides the profile", pngOf(ihdr, iccpChunk("x", vendorP3), encodeChunk("cICP", []byte{12, 13, 0, 1}), idat, iendChunk),
			pngOf(ihdr, encodeChunk("cICP", []byte{12, 13, 0, 1}), idat, iendChunk)},
		{"unknown profile", pngOf(ihdr, gama, iccpChunk("x", dciP3), idat, iendChunk),
			pngOf(ihdr, gama, idat, iendChunk)},
		{"RGB profile in a gray image", pngOf(gray, iccpChunk("x", vendorP3), grayIDAT, iendChunk),
			pngOf(gray, grayIDAT, iendChunk)},
		{"profile after PLTE", pngOf(ihdr, plte, iccpChunk("x", vendorP3), idat, iendChunk),
			pngOf(ihdr, idat, iendChunk)},
		{"second profile", pngOf(ihdr, iccpChunk("x", dciP3), iccpChunk("x", vendorP3), idat, iendChunk),
			pngOf(ihdr, idat, iendChunk)},
		{"broken stream", pngOf(ihdr, encodeChunk("iCCP", join([]byte("x\x00\x00"), deflate(vendorP3)[:40])), idat, iendChunk),
			pngOf(ihdr, idat, iendChunk)},
		{"compression method 1", pngOf(ihdr, encodeChunk("iCCP", join([]byte("x\x00\x01"), deflate(vendorP3))), idat, iendChunk),
			pngOf(ihdr, idat, iendChunk)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out := strip(t, tt.input)
			if !slices.Equal(chunkTypes(out), chunkTypes(tt.want)) {
				t.Errorf("Strip gives chunks %v, want %v", chunkTypes(out), chunkTypes(tt.want))
			}
			for _, typ := range []string{"iCCP", "sRGB", "cICP", "gAMA"} {
				if !bytes.Equal(chunkData(out, typ), chunkData(tt.want, typ)) {
					t.Errorf("%s data %q, want %q", typ, chunkData(out, typ), chunkData(tt.want, typ))
				}
			}
			if bytes.Contains(out, []byte("Secret")) || bytes.Contains(out, []byte("Pixel")) {
				t.Error("the profile's text survives")
			}
			if err := (Handler{}).Verify(budget(), bytes.NewReader(tt.input)); !errors.Is(err, format.ErrMetadataRemains) {
				t.Errorf("Verify of the input = %v, want ErrMetadataRemains", err)
			}
			if err := (Handler{}).Verify(budget(), bytes.NewReader(out)); err != nil {
				t.Errorf("Verify of the output: %v", err)
			}
			if fields, err := (Handler{}).Inspect(budget(), bytes.NewReader(out)); err != nil || len(fields) != 0 {
				t.Errorf("Inspect of the output = %v, %v", fields, err)
			}
		})
	}
}

// Inspect still reports a profile that Strip replaces: its own text goes.
func TestInspectKnownProfile(t *testing.T) {
	ihdr, idat := plainChunks()
	fields, err := (Handler{}).Inspect(budget(), bytes.NewReader(pngOf(ihdr, iccpChunk("Secret Corp camera", vendorP3), idat, iendChunk)))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(fields, func(f format.Field) bool {
		return f.Name == "ICC profile" && strings.HasPrefix(f.Value, "Secret Corp camera")
	}) {
		t.Errorf("Inspect = %v, want the ICC profile", fields)
	}
}

// Our iCCP chunks are a valid zlib stream of our profile, built without
// compress/flate.
func TestCanonicalICCP(t *testing.T) {
	for _, s := range []icc.Space{icc.DisplayP3, icc.AdobeRGB} {
		name, rest, _ := bytes.Cut(canonicalICCP()[s], []byte{0})
		if string(name) != s.String() || rest[0] != 0 {
			t.Fatalf("%v: name %q, method %d", s, name, rest[0])
		}
		zr, err := zlib.NewReader(bytes.NewReader(rest[1:]))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(zr)
		if err != nil || !bytes.Equal(got, icc.Profile(s)) {
			t.Errorf("%v: the stream holds %d bytes, %v", s, len(got), err)
		}
	}
}

// A profile that inflates past icc.MaxSize is dropped, not read whole.
func TestICCProfileTooLarge(t *testing.T) {
	ihdr, idat := plainChunks()
	big := append(bytes.Clone(vendorP3), make([]byte, icc.MaxSize)...)
	out := strip(t, pngOf(ihdr, iccpChunk("x", big), idat, iendChunk))
	if !slices.Equal(chunkTypes(out), []string{"IHDR", "IDAT", "IEND"}) {
		t.Errorf("Strip gives chunks %v", chunkTypes(out))
	}
}
