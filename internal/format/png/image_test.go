package png

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"testing"
)

// TestStripPreservesPixels strips random valid images of every color type and
// bit depth, interlaced or not, with random filter bytes, random row data and
// the stream split over random IDAT chunks (some empty). The standard
// library must decode the same pixels before and after.
func TestStripPreservesPixels(t *testing.T) {
	kinds := []struct{ colorType, depth byte }{
		{colorGray, 1}, {colorGray, 2}, {colorGray, 4}, {colorGray, 8}, {colorGray, 16},
		{colorRGB, 8}, {colorRGB, 16},
		{colorPalette, 1}, {colorPalette, 2}, {colorPalette, 4}, {colorPalette, 8},
		{colorGrayAlpha, 8}, {colorGrayAlpha, 16},
		{colorRGBA, 8}, {colorRGBA, 16},
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 3000 {
		k := kinds[rng.IntN(len(kinds))]
		h := header{
			width:      uint32(1 + rng.IntN(40)),
			height:     uint32(1 + rng.IntN(40)),
			depth:      k.depth,
			colorType:  k.colorType,
			interlaced: rng.IntN(2) == 1,
		}

		var stream []byte
		for _, p := range h.passes() {
			if p.width == 0 || p.height == 0 {
				continue
			}
			n := int(rowBytes(p.width, h.bitsPerPixel()))
			for range p.height {
				stream = append(stream, byte(rng.IntN(5)))
				for range n {
					stream = append(stream, byte(rng.Uint32()))
				}
			}
		}

		var interlace byte
		if h.interlaced {
			interlace = 1
		}
		file := pngOf(ihdrChunk(h.width, h.height, h.depth, h.colorType, interlace))
		if h.colorType == colorPalette {
			// A full palette, so every index is in range.
			palette := make([]byte, 3<<h.depth)
			for j := range palette {
				palette[j] = byte(rng.Uint32())
			}
			file = append(file, encodeChunk("PLTE", palette)...)
		}
		z := deflate(stream)
		for len(z) > 0 {
			n := min(len(z), rng.IntN(64))
			file = append(file, encodeChunk("IDAT", z[:n])...)
			z = z[n:]
		}
		file = append(file, iendChunk...)

		var out bytes.Buffer
		if err := (Handler{}).Strip(bytes.NewReader(file), &out); err != nil {
			t.Fatalf("image %d %+v: Strip: %v", i, h, err)
		}
		assertSamePixels(t, decode(t, out.Bytes()), decode(t, file))
	}
}

func TestFixIndexes(t *testing.T) {
	// 2-bit indexes 0, 3, 1, 2 with a 2-entry palette: 3 is out of range,
	// 2 is the canonical out-of-range value and stays.
	row := []byte{0b00_11_01_10}
	if n := fixIndexes(row, 4, 2, 2, true); n != 1 {
		t.Errorf("fixed %d indexes, want 1", n)
	}
	if row[0] != 0b00_10_01_10 {
		t.Errorf("row = %08b, want 00100110", row[0])
	}
	if n := fixIndexes(row, 4, 2, 2, false); n != 0 {
		t.Errorf("fixed row still has %d bad indexes", n)
	}

	row8 := binary.BigEndian.AppendUint32(nil, 0x00ff0102)
	if n := fixIndexes(row8, 4, 8, 2, true); n != 1 || row8[1] != 2 {
		t.Errorf("8-bit: fixed %d, row %v", n, row8)
	}
}
