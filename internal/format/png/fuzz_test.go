package png

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"slices"
	"testing"

	"traceless/internal/format"
	"traceless/internal/format/icc"
)

// FuzzHandler feeds arbitrary bytes to every handler method. Nothing may
// panic, and whenever Strip succeeds its output must verify as clean, report
// no metadata, and strip to itself.
//
// Each input runs twice: as it is, and with every chunk CRC repaired. Almost
// every mutation breaks a CRC, so without the repair the fuzzer rarely gets
// past the framing checks.
func FuzzHandler(f *testing.F) {
	ihdr, idat := plainChunks()
	f.Add(basePNG(f))
	f.Add(metadataPNG(f))
	f.Add(paddingPNG())
	f.Add(palettePNG())
	f.Add(interlacedPNG())
	f.Add(pngOf(ihdr, iccpChunk("x", vendorP3), encodeChunk("sRGB", []byte{0}), idat, iendChunk))
	f.Add(pngOf(ihdr, encodeChunk("iCCP", canonicalICCP()[icc.AdobeRGB]), idat, iendChunk))
	f.Add(pngOf(ihdr, iccpChunk("x", vendorSRGB), idat, iendChunk))
	f.Add(pngOf(ihdr, encodeChunk("gAMA", []byte{0, 0, 0xb1, 0x8f}), encodeChunk("cHRM", srgbChromaticities()),
		encodeChunk("sRGB", []byte{0}), encodeChunk("cICP", []byte{9, 16, 0, 1}),
		encodeChunk("mDCV", validMDCV()), encodeChunk("cLLI", make([]byte, 8)),
		encodeChunk("bKGD", make([]byte, 6)), encodeChunk("tRNS", make([]byte, 6)),
		encodeChunk("PLTE", make([]byte, 9)), idat, iendChunk))
	f.Add(pngOf(ihdrChunk(4, 1, 8, colorPalette, 0), encodeChunk("gAMA", []byte{0, 0, 0xb1, 0x8f}),
		encodeChunk("PLTE", []byte{9, 9, 9, 1, 2, 3}), encodeChunk("tRNS", []byte{1}), encodeChunk("bKGD", []byte{1}),
		encodeChunk("IDAT", deflate([]byte{0, 0, 1, 2, 3})), iendChunk))
	f.Add(pngOf(ihdrChunk(4, 1, 4, colorGray, 0), encodeChunk("tRNS", []byte{0, 15}), encodeChunk("bKGD", []byte{0, 3}),
		encodeChunk("IDAT", deflate([]byte{0, 0x0f, 0x37})), iendChunk))

	f.Fuzz(func(t *testing.T, data []byte) {
		checkHandler(t, data)
		checkHandler(t, fixCRCs(data))
		checkLimits(t, fixCRCs(data))
	})
}

func checkHandler(t *testing.T, data []byte) {
	t.Helper()
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
}

// fixCRCs returns a copy of b with the CRC of every complete chunk recomputed.
func fixCRCs(b []byte) []byte {
	b = bytes.Clone(b)
	for i := len(signature); i+12 <= len(b); {
		n := int(binary.BigEndian.Uint32(b[i:]))
		if n > len(b)-i-12 {
			break
		}
		binary.BigEndian.PutUint32(b[i+8+n:], crc32.ChecksumIEEE(b[i+4:i+8+n]))
		i += 12 + n
	}
	return b
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
	// Images need codecMemory before anything else, so half the draws start
	// there and the rest reach the text and EXIF reads before it.
	mem := int64(1) << (10 + s%12)
	if s&1 != 0 {
		mem += codecMemory
	}
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
