package jpeg

import (
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"traceless/internal/format"
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
		checkLimits(t, data)
		var h Handler
		h.Inspect(checked(t, format.DefaultLimits), bytes.NewReader(data))
		h.Verify(checked(t, format.DefaultLimits), bytes.NewReader(data))

		var out bytes.Buffer
		if err := h.Strip(checked(t, format.DefaultLimits), bytes.NewReader(data), &out); err != nil {
			return
		}
		if err := h.Verify(checked(t, format.DefaultLimits), bytes.NewReader(out.Bytes())); err != nil {
			t.Fatalf("stripped output fails Verify: %v", err)
		}
		fields, err := h.Inspect(checked(t, format.DefaultLimits), bytes.NewReader(out.Bytes()))
		if err != nil || len(fields) != 0 {
			t.Fatalf("stripped output: Inspect = %v, %v", fields, err)
		}
		var again bytes.Buffer
		if err := h.Strip(checked(t, format.DefaultLimits), bytes.NewReader(out.Bytes()), &again); err != nil {
			t.Fatalf("stripping the output again: %v", err)
		}
		if !bytes.Equal(out.Bytes(), again.Bytes()) {
			t.Fatal("Strip is not idempotent")
		}
	})
}

// checked returns a budget that must hold nothing once the test ends.
func checked(t *testing.T, l format.Limits) *format.Budget {
	b := format.NewBudget(context.Background(), l)
	t.Cleanup(func() {
		if b.Held() != 0 {
			t.Errorf("%d bytes still held", b.Held())
		}
	})
	return b
}

// smallLimits picks limits from data, from almost nothing up to what most
// test files need, so fuzzing reaches the point where each limit runs out.
func smallLimits(data []byte) format.Limits {
	s := crc32.ChecksumIEEE(data)
	mem := int64(1) << (10 + s%12)
	return format.Limits{
		Memory:       mem,
		Decompressed: 1 << (s >> 8 % 24),
		Work:         1 << (s >> 16 % 16),
		Output:       1 << (s >> 24 % 20),
		Depth:        1,
	}
}

// checkLimits runs every method under small limits. Each must give what the
// default limits give, or fail with ErrLimit; nothing may stay held. Verify
// runs on the input and on its Strip output, so it also meets canonical files.
func checkLimits(t *testing.T, data []byte) {
	var h Handler
	l := smallLimits(data)

	wantFields, wantErr := h.Inspect(budget(), bytes.NewReader(data))
	fields, err := h.Inspect(checked(t, l), bytes.NewReader(data))
	if !sameResult(err, wantErr) || err == nil && !slices.Equal(fields, wantFields) {
		t.Fatalf("Inspect under %+v = %v, %v; with the default limits %v, %v", l, fields, err, wantFields, wantErr)
	}

	var want, got bytes.Buffer
	wantErr = h.Strip(budget(), bytes.NewReader(data), &want)
	b := checked(t, l)
	err = h.Strip(b, bytes.NewReader(data), b.Writer(&got))
	if !sameResult(err, wantErr) || err == nil && !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatalf("Strip under %+v = %v; with the default limits %v", l, err, wantErr)
	}

	inputs := [][]byte{data}
	if wantErr == nil {
		inputs = append(inputs, want.Bytes())
	}
	for _, in := range inputs {
		wantErr := h.Verify(budget(), bytes.NewReader(in))
		if err := h.Verify(checked(t, l), bytes.NewReader(in)); !sameResult(err, wantErr) {
			t.Fatalf("Verify under %+v = %v; with the default limits %v", l, err, wantErr)
		}
	}
}

// sameResult reports whether a call under small limits agrees with the same
// call under the default ones: both pass, or the small one fails with
// ErrLimit, or both fail.
func sameResult(err, defaultErr error) bool {
	return errors.Is(err, format.ErrLimit) || (err == nil) == (defaultErr == nil)
}
