package jpeg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	stdjpeg "image/jpeg"
	"testing"

	"traceless/internal/format"
	"traceless/internal/format/icc"
	"traceless/internal/format/icc/icctest"
)

var (
	vendorP3 = icctest.Build(icctest.Profile{Version: 4, Colorants: icctest.DisplayP3, Curve: icctest.SRGBPara,
		Tags: map[string][]byte{"dmdd": icctest.Text("Pixel 7 Pro")}})
	vendorAdobe = icctest.Build(icctest.Profile{Version: 2, Colorants: icctest.AdobeRGB, Curve: icctest.Gamma22})
	vendorSRGB  = icctest.Build(icctest.Profile{Version: 2, Colorants: icctest.SRGB, Curve: icctest.SRGBTable})
)

// iccSegments splits profile into count APP2 segments, in the order of the
// sequence numbers given.
func iccSegments(profile []byte, seqs ...int) [][]byte {
	count := len(seqs)
	size := (len(profile) + count - 1) / count
	var out [][]byte
	for _, seq := range seqs {
		part := profile[min((seq-1)*size, len(profile)):min(seq*size, len(profile))]
		out = append(out, app(2, prefixICC, string([]byte{byte(seq), byte(count)}), string(part)))
	}
	return out
}

// app2 returns the data of the APP2 segments before the first scan.
func app2(b []byte) [][]byte {
	var out [][]byte
	for i := 2; i+4 <= len(b) && b[i] == 0xff && b[i+1] != markerSOS; {
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if b[i+1] == markerAPP0+2 {
			out = append(out, b[i+4:i+2+n])
		}
		i += 2 + n
	}
	return out
}

// Profiles of Display P3 and Adobe RGB are replaced by ours; others are
// dropped.
func TestICCProfile(t *testing.T) {
	base := baseJPEG(t)
	gray := strip(t, encode(t, image.NewGray(image.Rect(0, 0, 8, 8)))) // canonical: only the profile is found
	with := func(b []byte, segs [][]byte) []byte { return withSegments(b, segs...) }
	for _, tt := range []struct {
		name  string
		input []byte
		want  icc.Space // Unknown for no profile
	}{
		{"Display P3", with(base, iccSegments(vendorP3, 1)), icc.DisplayP3},
		{"Display P3 in parts, out of order", with(base, iccSegments(vendorP3, 2, 3, 1)), icc.DisplayP3},
		{"Adobe RGB", with(base, iccSegments(vendorAdobe, 1)), icc.AdobeRGB},
		{"sRGB", with(base, iccSegments(vendorSRGB, 1)), icc.Unknown},
		{"gray image", with(gray, iccSegments(vendorP3, 1)), icc.Unknown},
		{"missing part", with(base, iccSegments(vendorP3, 1, 2)[:1]), icc.Unknown},
		{"repeated part", with(base, append(iccSegments(vendorP3, 1, 2), iccSegments(vendorP3, 1, 2)[1])), icc.Unknown},
		{"sequence zero", with(base, [][]byte{app(2, prefixICC, "\x00\x01", string(vendorP3))}), icc.Unknown},
		{"too short", with(base, [][]byte{app(2, prefixICC, "\x01")}), icc.Unknown},
		{"short segment before", with(base, append([][]byte{app(2, prefixICC)}, iccSegments(vendorP3, 1)...)), icc.DisplayP3},
		{"short segment after", with(base, append(iccSegments(vendorP3, 1), app(2, prefixICC, "\x01"))), icc.DisplayP3},
		{"ours and another", with(base, [][]byte{app(2, string(iccData(icc.DisplayP3))), app(2, prefixICC, "\x01\x01", string(vendorP3))}), icc.Unknown},
		{"ours in a gray image", with(gray, [][]byte{app(2, string(iccData(icc.DisplayP3)))}), icc.Unknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out := strip(t, tt.input)
			var want [][]byte
			if tt.want != icc.Unknown {
				want = [][]byte{iccData(tt.want)}
			}
			if got := app2(out); len(got) != len(want) || len(want) == 1 && !bytes.Equal(got[0], want[0]) {
				t.Errorf("APP2 segments %d, want %v", len(got), tt.want)
			}
			if bytes.Contains(out, []byte("Secret")) || bytes.Contains(out, []byte("Pixel")) {
				t.Error("the profile's text survives")
			}
			if fields, err := (Handler{}).Inspect(budget(), bytes.NewReader(tt.input)); err != nil || len(fields) == 0 {
				t.Errorf("Inspect of the input = %v, %v, want fields", fields, err)
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
			if _, err := stdjpeg.Decode(bytes.NewReader(out)); err != nil {
				t.Errorf("image/jpeg can't decode the output: %v", err)
			}
		})
	}
}

// A profile larger than icc.MaxSize is dropped without being held whole.
func TestICCProfileTooLarge(t *testing.T) {
	big := append(bytes.Clone(vendorP3), make([]byte, icc.MaxSize)...)
	seqs := make([]int, len(big)/60000+1)
	for i := range seqs {
		seqs[i] = i + 1
	}
	out := strip(t, withSegments(baseJPEG(t), iccSegments(big, seqs...)...))
	if got := app2(out); len(got) != 0 {
		t.Errorf("%d APP2 segments", len(got))
	}
}
