package jpeg

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	stdjpeg "image/jpeg"
	"testing"

	"traceless/internal/format/exif/exiftest"
)

// testImage returns an image with distinct pixels.
func testImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x * 15), G: uint8(y * 15), B: uint8(x ^ y), A: 255})
		}
	}
	return img
}

// encode encodes img with the standard library: no metadata. Its headers are
// exactly what Strip writes, apart from JFIF.
func encode(tb testing.TB, img image.Image) []byte {
	tb.Helper()
	var buf bytes.Buffer
	if err := stdjpeg.Encode(&buf, img, &stdjpeg.Options{Quality: 90}); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

// baseJPEG is a 32x16 4:2:0 image without padding blocks.
func baseJPEG(tb testing.TB) []byte {
	return encode(tb, testImage(32, 16))
}

// seg builds a marker segment.
func seg(m byte, data []byte) []byte {
	return appendSegment(nil, m, data)
}

// app builds an APPn segment.
func app(n int, data ...string) []byte {
	return seg(byte(markerAPP0+n), []byte(join(data...)))
}

func join(parts ...string) string {
	var b bytes.Buffer
	for _, p := range parts {
		b.WriteString(p)
	}
	return b.String()
}

// withSegments inserts segments right after SOI.
func withSegments(base []byte, segs ...[]byte) []byte {
	out := append([]byte{}, base[:2]...)
	for _, s := range segs {
		out = append(out, s...)
	}
	return append(out, base[2:]...)
}

// beforeEOI inserts b right before the final EOI marker.
func beforeEOI(base []byte, b string) []byte {
	out := append([]byte{}, base[:len(base)-2]...)
	return append(append(out, b...), 0xff, markerEOI)
}

// jfifData returns a JFIF APP0 body with the given thumbnail size.
func jfifData(tw, th int, extra string) string {
	b := []byte(prefixJFIF)
	b = append(b, 1, 2, 1, 0, 72, 0, 72, byte(tw), byte(th))
	b = append(b, make([]byte, 3*tw*th)...)
	return string(b) + extra
}

// adobeData returns an Adobe APP14 body with transform t.
func adobeData(t byte, extra string) string {
	return prefixAdobe + string([]byte{0, 100, 0x80, 0, 0, 0, t}) + extra
}

// photoshopIPTC returns an APP13 body with IPTC By-line and City datasets.
func photoshopIPTC() string {
	var iim []byte
	for _, d := range []struct {
		dataset byte
		value   string
	}{{0, "\x00\x04"}, {80, "Jane Doe"}, {90, "Berlin"}} {
		iim = append(iim, 0x1c, 2, d.dataset)
		iim = binary.BigEndian.AppendUint16(iim, uint16(len(d.value)))
		iim = append(iim, d.value...)
	}
	res := []byte("8BIM\x04\x04\x00\x00")
	res = binary.BigEndian.AppendUint32(res, uint32(len(iim)))
	res = append(res, iim...)
	if len(res)%2 == 1 {
		res = append(res, 0)
	}
	return prefixPhotoshop + string(res)
}

// metadataJPEG returns baseJPEG carrying every kind of metadata the handler
// knows, plus a private segment and trailing data.
func metadataJPEG(tb testing.TB) []byte {
	tb.Helper()
	out := withSegments(baseJPEG(tb),
		app(0, jfifData(0, 0, "")),
		app(1, prefixEXIF, string(exiftest.Photo(binary.BigEndian))),
		app(1, prefixXMP, "<x:xmpmeta>GPS 52.5200,13.4050</x:xmpmeta>"),
		app(2, prefixICC, "\x01\x01fake ICC profile"),
		app(13, photoshopIPTC()),
		seg(markerCOM, []byte("taken at home")),
		app(11, "PRIVATE\x00secret app data"),
	)
	return append(out, "SECRET-TRAILER"...)
}

// built describes a JPEG assembled by hand, for features the standard
// library encoder doesn't produce. Its image data is all zero blocks, coded
// with the standard tables.
type built struct {
	width, height int
	sampling      []byte // per component, 0x11 if nil
	ids           []byte // component IDs, 1, 2, ... if nil
	segs          [][]byte
	restart       int    // restart interval, 0 for none
	data          []byte // image data, generated if nil
}

func (b built) bytes() []byte {
	n := max(len(b.sampling), len(b.ids), 1)
	sof := []byte{8}
	sof = binary.BigEndian.AppendUint16(sof, uint16(b.height))
	sof = binary.BigEndian.AppendUint16(sof, uint16(b.width))
	sof = append(sof, byte(n))
	sos := []byte{byte(n)}
	for i := range n {
		id, s := byte(i+1), byte(0x11)
		if b.ids != nil {
			id = b.ids[i]
		}
		if b.sampling != nil {
			s = b.sampling[i]
		}
		sof = append(sof, id, s, 0)
		sos = append(sos, id, byte(0x11*min(i, 1)))
	}
	sos = append(sos, 0, 63, 0)

	dht := appendDHT(nil, 0, 0, stdLumaDC)
	dht = appendDHT(dht, 1, 0, stdLumaAC)
	if n > 1 {
		dht = appendDHT(dht, 0, 1, stdChromaDC)
		dht = appendDHT(dht, 1, 1, stdChromaAC)
	}

	out := []byte{0xff, markerSOI}
	for _, s := range b.segs {
		out = append(out, s...)
	}
	out = append(out, seg(markerDQT, append([]byte{0}, bytes.Repeat([]byte{1}, 64)...))...)
	out = append(out, seg(markerSOF0, sof)...)
	out = append(out, seg(markerDHT, dht)...)
	if b.restart > 0 {
		out = append(out, seg(markerDRI, binary.BigEndian.AppendUint16(nil, uint16(b.restart)))...)
	}
	out = append(out, seg(markerSOS, sos)...)
	data := b.data
	if data == nil {
		data = zeroData(sof, b.restart)
	}
	out = append(out, data...)
	return append(out, 0xff, markerEOI)
}

// zeroData codes all-zero blocks for the frame in sof.
func zeroData(sof []byte, restart int) []byte {
	var hd header
	if err := hd.parseSOF(sof); err != nil {
		panic(err)
	}
	f := hd.frame
	across, down := f.mcus()
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	bw := &bitWriter{w: w}
	var pred [4]int32
	for i := range across * down {
		if restart > 0 && i > 0 && i%restart == 0 {
			bw.flush()
			w.Write([]byte{0xff, byte(markerRST0 + (i/restart-1)%8)})
			pred = [4]int32{}
		}
		for ci, c := range f.comps {
			blocks := c.h * c.v
			if len(f.comps) == 1 {
				blocks = 1
			}
			for range blocks {
				enc := 2 * min(ci, 1)
				bw.encodeBlock(stdEncoders[enc], stdEncoders[enc+1], &pred[ci], &block{})
			}
		}
	}
	bw.flush()
	w.Flush()
	return buf.Bytes()
}
