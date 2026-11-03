package png

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"testing"

	"traceless/internal/format"
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
		if err := (Handler{}).Strip(budget(), bytes.NewReader(file), &out); err != nil {
			t.Fatalf("image %d %+v: Strip: %v", i, h, err)
		}
		assertSamePixels(t, decode(t, out.Bytes()), decode(t, file))
	}
}

// TestMarkIndexes compares markIndexes with a simple bit-by-bit reference on
// random rows of every palette depth.
func TestMarkIndexes(t *testing.T) {
	index := func(row []byte, i, depth int) int {
		bit := i * depth
		return int(row[bit/8]>>(8-depth-bit%8)) & (1<<depth - 1)
	}
	rng := rand.New(rand.NewPCG(3, 4))
	for range 100_000 {
		depth := []int{1, 2, 4, 8}[rng.IntN(4)]
		width := 1 + rng.IntN(37)
		paletteLen := 1 + rng.IntN(1<<depth)
		row := make([]byte, (width*depth+7)/8)
		for j := range row {
			row[j] = byte(rng.Uint32())
		}

		var want [256]bool
		bad := false
		for i := range width {
			if v := index(row, i, depth); v < paletteLen {
				want[v] = true
			} else {
				bad = true
			}
		}
		var used [256]bool
		err := markIndexes(row, uint32(width), uint8(depth), paletteLen, &used)
		if bad != errors.Is(err, format.ErrMalformed) {
			t.Fatalf("depth %d, %d entries, row %08b: err = %v, out of range: %v", depth, paletteLen, row, err, bad)
		}
		if !bad && used != want {
			t.Fatalf("depth %d, %d entries, row %08b: used indexes differ", depth, paletteLen, row)
		}
	}
}
