package png

import "encoding/binary"

// Color types from the PNG spec.
const (
	colorGray      = 0
	colorRGB       = 2
	colorPalette   = 3
	colorGrayAlpha = 4
	colorRGBA      = 6
)

// maxRowBytes caps the size of one decoded row. Stripping keeps a few rows in
// memory, so this bounds memory use whatever width the header claims. 4 MiB
// still allows RGBA images over a million pixels wide.
const maxRowBytes = 4 << 20

// maxSide caps the width and the height at the user limits a default libpng
// build refuses past. It bounds the rows, whose cost doesn't depend on their
// size: a 1x2^28 image would take ~17 s.
const maxSide = 1_000_000

// maxPixels caps the image area (16384 x 16384). Work grows with the pixel
// count the header claims, not with the file size, and deflate compresses
// about 1000:1. Even with the cap, a 2 MB file can decode to 2 GiB and take
// about 30 seconds to strip, so a server needs its own time budget.
const maxPixels = 1 << 28

// header holds the parsed IHDR chunk.
type header struct {
	width, height uint32
	depth         uint8
	colorType     uint8
	interlaced    bool
}

func parseHeader(b []byte) (header, error) {
	if len(b) != 13 {
		return header{}, malformed("IHDR length %d, want 13", len(b))
	}
	h := header{
		width:      binary.BigEndian.Uint32(b[0:4]),
		height:     binary.BigEndian.Uint32(b[4:8]),
		depth:      b[8],
		colorType:  b[9],
		interlaced: b[12] == 1,
	}
	if h.width == 0 || h.height == 0 || h.width > maxChunkLen || h.height > maxChunkLen {
		return header{}, malformed("invalid image size %dx%d", h.width, h.height)
	}
	if !validDepth(h.colorType, h.depth) {
		return header{}, malformed("invalid bit depth %d for color type %d", h.depth, h.colorType)
	}
	if b[10] != 0 || b[11] != 0 || b[12] > 1 {
		return header{}, malformed("invalid compression, filter or interlace method")
	}
	if h.width > maxSide || h.height > maxSide {
		return header{}, unsupported("image side over %d pixels (%dx%d)", maxSide, h.width, h.height)
	}
	if rowBytes(h.width, h.bitsPerPixel()) > maxRowBytes {
		return header{}, unsupported("image too wide (%d pixels)", h.width)
	}
	if uint64(h.width)*uint64(h.height) > maxPixels {
		return header{}, unsupported("image too large (%dx%d)", h.width, h.height)
	}
	return h, nil
}

func validDepth(colorType, depth uint8) bool {
	switch colorType {
	case colorGray:
		return depth == 1 || depth == 2 || depth == 4 || depth == 8 || depth == 16
	case colorPalette:
		return depth == 1 || depth == 2 || depth == 4 || depth == 8
	case colorRGB, colorGrayAlpha, colorRGBA:
		return depth == 8 || depth == 16
	}
	return false
}

func (h header) channels() int {
	switch h.colorType {
	case colorRGB:
		return 3
	case colorGrayAlpha:
		return 2
	case colorRGBA:
		return 4
	}
	return 1
}

func (h header) bitsPerPixel() int {
	return h.channels() * int(h.depth)
}

// filterOffset is the byte distance filters use to find the "left" pixel.
func (h header) filterOffset() int {
	return max(1, h.bitsPerPixel()/8)
}

// rowBytes returns the size of one row of width pixels, without the filter byte.
func rowBytes(width uint32, bitsPerPixel int) int64 {
	return (int64(width)*int64(bitsPerPixel) + 7) / 8
}

// adam7 describes the seven passes of an interlaced image: the first pixel of
// each pass and the step between pixels.
var adam7 = [7]struct{ x0, y0, dx, dy uint32 }{
	{0, 0, 8, 8}, {4, 0, 8, 8}, {0, 4, 4, 8}, {2, 0, 4, 4},
	{0, 2, 2, 4}, {1, 0, 2, 2}, {0, 1, 1, 2},
}

// pass is one sub-image stored in the pixel stream.
type pass struct{ width, height uint32 }

// passes returns the sub-images in stream order: the whole image, or the
// seven Adam7 passes, some of which may be empty.
func (h header) passes() []pass {
	if !h.interlaced {
		return []pass{{h.width, h.height}}
	}
	ps := make([]pass, 0, len(adam7))
	for _, p := range adam7 {
		ps = append(ps, pass{span(h.width, p.x0, p.dx), span(h.height, p.y0, p.dy)})
	}
	return ps
}

// span counts the pixels at start, start+step, ... below size.
func span(size, start, step uint32) uint32 {
	if size <= start {
		return 0
	}
	return (size - start + step - 1) / step
}

// renderingChunks lists the ancillary chunks that may be kept. Every other
// ancillary chunk is dropped, including some that look harmless:
//
//   - iCCP: ICC profiles carry descriptions, copyright strings and creation
//     dates that can identify a device or editing software. A profile of a
//     space package icc knows is replaced by ours (see iccp); viewers fall
//     back to sRGB, cHRM or gAMA for the others.
//   - pHYs: the pixel density only sets the print size, and values such as
//     144 dpi identify the device (Retina screenshots).
//   - sBIT: the original sample depth reveals the source; viewers don't need it.
//
// What stays can still hold a few dozen bytes chosen by whoever made the
// file: gAMA, cHRM, mDCV and cLLI values are only range-checked, and viewers
// use them, so they can't be dropped without changing colors.
var renderingChunks = map[string]bool{
	"tRNS": true, "gAMA": true, "cHRM": true, "sRGB": true,
	"bKGD": true, "cICP": true, "mDCV": true, "cLLI": true,
}

// fixedSize holds the exact data length of fixed-size rendering chunks.
var fixedSize = map[string]int{
	"gAMA": 4, "cHRM": 32, "sRGB": 1, "cICP": 4, "mDCV": 24, "cLLI": 8,
}

// colorSpaceChunks must come before PLTE. They are buffered until PLTE or the
// image data, because which of them a viewer uses depends on the others.
var colorSpaceChunks = map[string]bool{
	"gAMA": true, "cHRM": true, "sRGB": true, "cICP": true, "mDCV": true, "cLLI": true,
}

// maxRenderingLen is the largest valid rendering chunk (tRNS for 256 colors).
const maxRenderingLen = 256

// layout tracks chunk order to decide whether a rendering chunk is valid
// where it appears. A decoder ignores misplaced, duplicate, wrongly sized or
// out-of-range ancillary chunks, so such chunks don't affect the image and
// can only carry hidden data.
type layout struct {
	hdr        header
	paletteLen int // number of PLTE entries, 0 if none yet
	sawPalette bool
	sawImage   bool
	seen       map[string]bool
}

// checkRendering returns why a rendering chunk must be dropped, or "" if it
// can be kept.
func (l *layout) checkRendering(typ string, length uint32) string {
	switch {
	case l.seen[typ]:
		return "duplicate"
	case l.sawImage:
		return "after image data"
	case l.sawPalette && colorSpaceChunks[typ]:
		return "after PLTE"
	case (typ == "tRNS" || typ == "bKGD") && l.hdr.colorType == colorPalette && !l.sawPalette:
		return "before PLTE"
	}
	if !l.validLength(typ, length) {
		return "invalid length"
	}
	return ""
}

func (l *layout) validLength(typ string, length uint32) bool {
	n := int(length)
	if size, ok := fixedSize[typ]; ok {
		return n == size
	}
	ct := l.hdr.colorType
	switch typ {
	case "bKGD":
		return n == map[uint8]int{colorGray: 2, colorRGB: 6, colorPalette: 1, colorGrayAlpha: 2, colorRGBA: 6}[ct]
	case "tRNS":
		switch ct {
		case colorGray:
			return n == 2
		case colorRGB:
			return n == 6
		case colorPalette:
			return n >= 1 && n <= l.paletteLen
		}
	}
	return false
}

// checkValues returns why a rendering chunk's values are invalid, or "".
// Decoders ignore chunks with out-of-range values, so they can only carry
// hidden data.
func (l *layout) checkValues(typ string, data []byte) string {
	switch typ {
	case "sRGB":
		if data[0] > 3 {
			return "invalid rendering intent"
		}
	case "gAMA":
		// libpng ignores gamma values outside this range.
		if g := binary.BigEndian.Uint32(data); g < 16 || g > 625000000 {
			return "gamma out of range"
		}
	case "cHRM":
		// x,y pairs in units of 0.00001: white point, red, green, blue.
		if !validXY(uints(data, 4), 100000, 0) {
			return "invalid chromaticities"
		}
	case "mDCV":
		// x,y pairs in units of 0.00002: red, green, blue, white point;
		// then two luminance values.
		if !validXY(uints(data[:16], 2), 50000, 3) {
			return "invalid chromaticities"
		}
	case "cICP":
		// Only RGB (matrix 0) is valid for PNG; the range flag is 0 or 1.
		if data[2] != 0 || data[3] > 1 {
			return "invalid values"
		}
	case "bKGD", "tRNS":
		if l.hdr.colorType == colorPalette {
			if typ == "bKGD" && int(data[0]) >= l.paletteLen {
				return "invalid palette index"
			}
			return ""
		}
		// Gray or RGB samples, two bytes each, must fit the bit depth.
		for i := 0; i+1 < len(data); i += 2 {
			if int(binary.BigEndian.Uint16(data[i:])) >= 1<<l.hdr.depth {
				return "sample out of range"
			}
		}
	}
	return ""
}

// validXY checks x,y chromaticity pairs in fixed point, where one stands for
// 1.0, with the white point's pair at index white. A chromaticity lies in
// [0, 1] with x + y <= 1, and a white point needs y > 0 (at least 5 units,
// like libpng). Decoders ignore chunks with other values.
func validXY(xy []uint32, one uint32, white int) bool {
	for i := 0; i+1 < len(xy); i += 2 {
		x, y := xy[i], xy[i+1]
		if x > one || y > one-x {
			return false
		}
		if i/2 == white && y < 5 {
			return false
		}
	}
	return true
}

// uints decodes big-endian unsigned integers of size bytes each.
func uints(data []byte, size int) []uint32 {
	v := make([]uint32, 0, len(data)/size)
	for i := 0; i+size <= len(data); i += size {
		if size == 2 {
			v = append(v, uint32(binary.BigEndian.Uint16(data[i:])))
		} else {
			v = append(v, binary.BigEndian.Uint32(data[i:]))
		}
	}
	return v
}

// overridden returns why a buffered color space chunk is unused given the
// others, or "". cICP takes priority over iCCP, then sRGB, then gAMA and
// cHRM (PNG third edition, 4.3); mDCV and cLLI only apply together with
// cICP. The spec says iCCP and sRGB shouldn't both be there, and libpng
// uses whichever comes first; keeping only iCCP makes every viewer agree.
func overridden(typ string, present map[string]bool) string {
	switch typ {
	case "iCCP", "sRGB", "gAMA", "cHRM":
		if present["cICP"] {
			return "overridden by cICP"
		}
		if typ != "iCCP" && present["iCCP"] {
			return "overridden by iCCP"
		}
		if typ != "iCCP" && typ != "sRGB" && present["sRGB"] {
			return "overridden by sRGB"
		}
	case "mDCV", "cLLI":
		if !present["cICP"] {
			return "unused without cICP"
		}
	}
	return ""
}
