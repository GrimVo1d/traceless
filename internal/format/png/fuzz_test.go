package png

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
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
	})
}

func checkHandler(t *testing.T, data []byte) {
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
