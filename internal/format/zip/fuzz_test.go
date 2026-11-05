package zip

import (
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"slices"
	"testing"

	"traceless/internal/format"
)

// FuzzHandler feeds arbitrary bytes to every handler method. Nothing may
// panic, and whenever Strip succeeds its output must verify as clean, report
// no metadata, and strip to itself.
func FuzzHandler(f *testing.F) {
	img := stripWith(f, pngHandler(), metadataPNG(f))
	f.Add(metadataZIP(f))
	f.Add(cleanZIP(f))
	f.Add(rawZIP(""))
	f.Add(rawZIP("comment", rawEntry{name: "a.png", data: img, stored: true, date: 0x5821, comment: []byte("c")}))
	f.Add(rawZIP("", rawEntry{name: "d/", stored: true, external: 0o40755 << 16}, rawEntry{name: "d/e", data: nil},
		rawEntry{name: "d/.DS_Store", data: []byte("x")}, rawEntry{name: "d/a.png", data: img, extra: []byte{0x55, 0x78, 4, 0, 1, 0, 2, 0}}))
	f.Add(rawZIP("", rawEntry{name: "a.png", data: img, descriptor: true, hidden: []byte("h")}))
	f.Add(rawZIP("", rawEntry{name: "in.zip", data: cleanZIP(f)}))

	f.Fuzz(func(t *testing.T, data []byte) {
		checkLimits(t, data)
		h := handler()
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
	// Half the draws start from what an archive of images needs before
	// anything else (our buffers and codecs, and the PNG codec).
	mem := int64(1) << (10 + s%12)
	if s&1 != 0 {
		mem += scratchLen + readerMemory + 2*writerMemory
	}
	return format.Limits{
		Memory:       mem,
		Decompressed: 1 << (s >> 8 % 24),
		Work:         1 << (s >> 16 % 16),
		Output:       1 << (s >> 24 % 20),
		Depth:        int(s>>28%3) + 1,
	}
}

// checkLimits runs every method under small limits. Each must give what the
// default limits give, or fail with ErrLimit; nothing may stay held. Verify
// runs on the input and on its Strip output, so it also meets canonical files.
func checkLimits(t *testing.T, data []byte) {
	h := handler()
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
