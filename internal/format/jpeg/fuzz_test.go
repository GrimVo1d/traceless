package jpeg

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// FuzzHandler feeds arbitrary bytes to every handler method. Nothing may
// panic, and whenever Strip succeeds its output must verify as clean, report
// no metadata, and strip to itself.
func FuzzHandler(f *testing.F) {
	f.Add(baseJPEG(f))
	f.Add(metadataJPEG(f))
	f.Add(encode(f, testImage(17, 9)))
	f.Add(built{width: 16, height: 8, restart: 1}.bytes())
	f.Add(built{width: 33, height: 7, sampling: []byte{0x21, 0x11, 0x11}, segs: [][]byte{app(14, adobeData(0, ""))}}.bytes())
	f.Add(built{width: 8, height: 8, sampling: []byte{0x11, 0x11, 0x11, 0x11}, segs: [][]byte{app(14, adobeData(2, ""))}}.bytes())
	f.Add(withSegments(baseJPEG(f), app(0, jfifData(1, 1, "x")), app(1, string(orientationSegment(6)))))
	for _, name := range []string{"prog-custom.jpg", "prog-restart.jpg", "prog-gray.jpg", "prog-cmyk.jpg", "multiscan-pair.jpg"} {
		b, err := os.ReadFile(filepath.Join("testdata", "scans", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var h Handler
		h.Inspect(bytes.NewReader(data))
		h.Verify(bytes.NewReader(data))

		var out bytes.Buffer
		if err := h.Strip(bytes.NewReader(data), &out); err != nil {
			return
		}
		if err := h.Verify(bytes.NewReader(out.Bytes())); err != nil {
			t.Fatalf("stripped output fails Verify: %v", err)
		}
		fields, err := h.Inspect(bytes.NewReader(out.Bytes()))
		if err != nil || len(fields) != 0 {
			t.Fatalf("stripped output: Inspect = %v, %v", fields, err)
		}
		var again bytes.Buffer
		if err := h.Strip(bytes.NewReader(out.Bytes()), &again); err != nil {
			t.Fatalf("stripping the output again: %v", err)
		}
		if !bytes.Equal(out.Bytes(), again.Bytes()) {
			t.Fatal("Strip is not idempotent")
		}
	})
}
