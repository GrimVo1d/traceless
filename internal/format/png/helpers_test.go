package png

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	stdpng "image/png"
	"testing"

	"traceless/internal/format"
	"traceless/internal/format/exif/exiftest"
)

// ihdrEnd is the offset right after the IHDR chunk: 8 bytes of signature
// plus 8 (length, type) + 13 (data) + 4 (CRC).
const ihdrEnd = 33

var iendChunk = encodeChunk("IEND", nil)

// testImage returns a small image with distinct pixels.
func testImage() image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := range 4 {
		for x := range 4 {
			img.Set(x, y, color.NRGBA{R: uint8(x * 60), G: uint8(y * 60), B: 128, A: 255})
		}
	}
	return img
}

// basePNG encodes testImage with the standard library: no metadata.
func basePNG(tb testing.TB) []byte {
	tb.Helper()
	var buf bytes.Buffer
	if err := stdpng.Encode(&buf, testImage()); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

// encodeChunk builds a chunk with a correct length and CRC.
func encodeChunk(typ string, data []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(len(data)))
	b.WriteString(typ)
	b.Write(data)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	return b.Bytes()
}

// withChunks inserts chunks right after IHDR.
func withChunks(base []byte, chunks ...[]byte) []byte {
	out := append([]byte{}, base[:ihdrEnd]...)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return append(out, base[ihdrEnd:]...)
}

// pngOf builds a file from complete chunks.
func pngOf(chunks ...[]byte) []byte {
	return join(append([][]byte{[]byte(signature)}, chunks...)...)
}

func ihdrChunk(width, height uint32, depth, colorType, interlace byte) []byte {
	data := binary.BigEndian.AppendUint32(nil, width)
	data = binary.BigEndian.AppendUint32(data, height)
	return encodeChunk("IHDR", append(data, depth, colorType, 0, 0, interlace))
}

// testStream returns testImage as an unfiltered RGB pixel stream: each row is
// filter type 0 followed by the pixels.
func testStream() []byte {
	var s []byte
	for y := range 4 {
		s = append(s, 0)
		for x := range 4 {
			s = append(s, byte(x*60), byte(y*60), 128)
		}
	}
	return s
}

// plainChunks returns the IHDR and IDAT chunks of testImage built by hand.
func plainChunks() (ihdr, idat []byte) {
	return ihdrChunk(4, 4, 8, colorRGB, 0), encodeChunk("IDAT", deflate(testStream()))
}

func deflate(b []byte) []byte {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

func join(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

// metadataPNG returns testImage carrying every kind of metadata the handler
// knows, plus a private chunk and trailing data.
func metadataPNG(tb testing.TB) []byte {
	tb.Helper()
	out := withChunks(basePNG(tb),
		encodeChunk("iCCP", join([]byte("Pixel 7 Pro camera\x00\x00"), deflate([]byte("fake ICC profile")))),
		encodeChunk("tEXt", []byte("Author\x00Jane Doe")),
		encodeChunk("zTXt", join([]byte("Comment\x00\x00"), deflate([]byte("taken at home")))),
		encodeChunk("iTXt", join([]byte("XML:com.adobe.xmp\x00\x01\x00en\x00\x00"),
			deflate([]byte("<x:xmpmeta>GPS 52.5200,13.4050</x:xmpmeta>")))),
		encodeChunk("eXIf", exiftest.Photo(binary.BigEndian)),
		encodeChunk("tIME", []byte{0x07, 0xe8, 5, 1, 12, 30, 0}),
		encodeChunk("prVt", []byte("private app data")),
	)
	return append(out, "SECRET-TRAILER"...)
}

// paddingPNG returns a 3x2 1-bit grayscale image whose rows have their five
// unused bits set.
func paddingPNG() []byte {
	stream := []byte{0, 0b101_11111, 0, 0b010_10101}
	return pngOf(ihdrChunk(3, 2, 1, colorGray, 0), encodeChunk("IDAT", deflate(stream)), iendChunk)
}

// palettePNG returns a 5x2 2-bit palette image with transparency and a
// background color. Rows have six unused bits, left at zero.
func palettePNG() []byte {
	stream := []byte{
		0, 0b00_01_10_00, 0b01_000000,
		0, 0b10_10_01_01, 0b00_000000,
	}
	return pngOf(
		ihdrChunk(5, 2, 2, colorPalette, 0),
		encodeChunk("PLTE", []byte{255, 0, 0, 0, 255, 0, 0, 0, 255}),
		encodeChunk("tRNS", []byte{128}),
		encodeChunk("bKGD", []byte{1}),
		encodeChunk("IDAT", deflate(stream)),
		iendChunk,
	)
}

// interlacedPNG returns a 5x5 RGB Adam7 image.
func interlacedPNG() []byte {
	const size = 5
	var stream []byte
	for _, p := range adam7 {
		for y := p.y0; y < size; y += p.dy {
			if p.x0 >= size {
				break
			}
			stream = append(stream, 0)
			for x := p.x0; x < size; x += p.dx {
				stream = append(stream, byte(x*50), byte(y*50), byte(x+y))
			}
		}
	}
	return pngOf(ihdrChunk(size, size, 8, colorRGB, 1), encodeChunk("IDAT", deflate(stream)), iendChunk)
}

// chunkTypes lists the chunk types of a well-formed file.
func chunkTypes(b []byte) []string {
	var types []string
	for i := len(signature); i+8 <= len(b); {
		n := int(binary.BigEndian.Uint32(b[i:]))
		types = append(types, string(b[i+4:i+8]))
		i += 12 + n
	}
	return types
}

// chunkData returns the data of the first chunk of type typ in a well-formed
// file, or nil if there is none.
func chunkData(b []byte, typ string) []byte {
	for i := len(signature); i+8 <= len(b); {
		n := int(binary.BigEndian.Uint32(b[i:]))
		if string(b[i+4:i+8]) == typ {
			return b[i+8 : i+8+n]
		}
		i += 12 + n
	}
	return nil
}

// budget returns a budget with the default limits for one handler call.
func budget() *format.Budget {
	return format.NewBudget(context.Background(), format.DefaultLimits)
}
