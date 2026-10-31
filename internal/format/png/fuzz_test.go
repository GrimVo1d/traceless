package png

import (
	"bytes"
	"testing"
)

// FuzzHandler feeds arbitrary bytes to every handler method. Nothing may
// panic, and whenever Strip succeeds its output must verify as clean, report
// no metadata, and strip to itself.
func FuzzHandler(f *testing.F) {
	f.Add(basePNG(f))
	f.Add(metadataPNG(f))
	f.Add(paddingPNG())
	f.Add(palettePNG())
	f.Add(interlacedPNG())

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
