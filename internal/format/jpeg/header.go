package jpeg

import (
	"bytes"
	"encoding/binary"
	"io"
	"slices"

	"traceless/internal/format/exif"
)

const (
	// maxBlocksPerMCU is the limit ITU T.81 B.2.3 sets for interleaved scans.
	maxBlocksPerMCU = 10

	// maxSampling is the largest sampling factor a component may have.
	maxSampling = 4
)

// Identifiers at the start of APPn segments.
const (
	prefixJFIF      = "JFIF\x00"
	prefixJFXX      = "JFXX\x00"
	prefixEXIF      = "Exif\x00\x00"
	prefixXMP       = "http://ns.adobe.com/xap/1.0/\x00"
	prefixXMPExt    = "http://ns.adobe.com/xmp/extension/\x00"
	prefixICC       = "ICC_PROFILE\x00"
	prefixMPF       = "MPF\x00"
	prefixPhotoshop = "Photoshop 3.0\x00"
	prefixAdobe     = "Adobe"

	// jfifLen and adobeLen are the sizes of the fixed parts decoders read.
	jfifLen  = 14
	adobeLen = 12
)

type component struct {
	id     byte
	h, v   int  // sampling factors
	tq     byte // quantization table
	td, ta byte // DC and AC Huffman tables, from the scan header
	bw, bh int  // size in blocks
}

type frame struct {
	width, height int
	comps         []component
	hmax, vmax    int
}

// jfif holds the values kept from a JFIF APP0 segment: the pixel density.
type jfif struct {
	units  byte
	xd, yd uint16
}

// header collects everything before the scan. Tables are counted per slot,
// so definitions that are replaced or never referenced can be reported.
type header struct {
	frame       *frame
	baseline    bool // SOF0: Huffman tables 0 and 1 only
	quant       [4]*[64]uint16
	quantDefs   [4]int
	huff        [2][4]*decoder // [0] DC, [1] AC
	huffDefs    [2][4]int
	restart     int // restart interval in MCUs, 0 for none
	restartDefs int
	jfif        *jfif
	adobe       *byte // Adobe color transform
	sawEXIF     bool  // the first EXIF segment was seen
	orientation int   // its Orientation tag, 0 if none

	// Decided by checkScan.
	keepAdobe bool
}

func (hd *header) parseDQT(data []byte) error {
	for len(data) > 0 {
		pq, tq := data[0]>>4, data[0]&15
		if pq > 1 || tq > 3 {
			return malformed("invalid DQT table %d/%d", pq, tq)
		}
		size := 64 * int(pq+1)
		if len(data) < 1+size {
			return malformed("truncated DQT segment")
		}
		var t [64]uint16
		for i := range t {
			if pq == 0 {
				t[i] = uint16(data[1+i])
			} else {
				t[i] = binary.BigEndian.Uint16(data[1+2*i:])
			}
			if t[i] == 0 {
				return malformed("quantization value 0")
			}
		}
		hd.quant[tq] = &t
		hd.quantDefs[tq]++
		data = data[1+size:]
	}
	return nil
}

func (hd *header) parseDHT(data []byte) error {
	for len(data) > 0 {
		if len(data) < 17 {
			return malformed("truncated DHT segment")
		}
		tc, th := data[0]>>4, data[0]&15
		if tc > 1 || th > 3 {
			return malformed("invalid DHT table %d/%d", tc, th)
		}
		var s huffSpec
		copy(s.counts[:], data[1:17])
		n := 0
		for _, c := range s.counts {
			n += int(c)
		}
		if n == 0 || n > 256 || len(data) < 17+n {
			return malformed("DHT table with %d symbols", n)
		}
		s.symbols = data[17 : 17+n]
		if tc == 0 && slices.Max(s.symbols) > 15 {
			// libjpeg refuses such tables, so viewers show nothing.
			return malformed("DC table symbol %d", slices.Max(s.symbols))
		}
		d, err := newDecoder(s)
		if err != nil {
			return err
		}
		hd.huff[tc][th] = d
		hd.huffDefs[tc][th]++
		data = data[17+n:]
	}
	return nil
}

func (hd *header) parseDRI(data []byte) error {
	if len(data) != 2 {
		return malformed("DRI segment of %d bytes", len(data))
	}
	hd.restart = int(binary.BigEndian.Uint16(data))
	hd.restartDefs++
	return nil
}

func (hd *header) parseSOF(data []byte) error {
	if hd.frame != nil {
		return malformed("more than one frame header")
	}
	if len(data) < 6 {
		return malformed("truncated SOF segment")
	}
	if data[0] != 8 {
		return unsupported("%d-bit samples", data[0])
	}
	f := &frame{
		height: int(binary.BigEndian.Uint16(data[1:])),
		width:  int(binary.BigEndian.Uint16(data[3:])),
	}
	n := int(data[5])
	switch {
	case len(data) != 6+3*n:
		return malformed("SOF segment of %d bytes for %d components", len(data), n)
	case f.height == 0:
		return unsupported("image height set by a DNL marker")
	case f.width == 0:
		return malformed("image width 0")
	case n == 0:
		return malformed("no image components")
	case n == 2 || n > 4:
		return unsupported("%d image components", n)
	}

	blocks := 0
	for i := range n {
		c := component{id: data[6+3*i], h: int(data[7+3*i] >> 4), v: int(data[7+3*i] & 15), tq: data[8+3*i]}
		if c.h < 1 || c.h > maxSampling || c.v < 1 || c.v > maxSampling {
			return malformed("sampling factors %dx%d", c.h, c.v)
		}
		if c.tq > 3 {
			return malformed("quantization table %d", c.tq)
		}
		if slices.ContainsFunc(f.comps, func(o component) bool { return o.id == c.id }) {
			return malformed("duplicate component %d", c.id)
		}
		blocks += c.h * c.v
		f.hmax, f.vmax = max(f.hmax, c.h), max(f.vmax, c.v)
		f.comps = append(f.comps, c)
	}
	if n > 1 && blocks > maxBlocksPerMCU {
		return malformed("%d blocks per MCU", blocks)
	}
	for i := range f.comps {
		c := &f.comps[i]
		c.bw = ceilDiv(ceilDiv(f.width*c.h, f.hmax), 8)
		c.bh = ceilDiv(ceilDiv(f.height*c.v, f.vmax), 8)
	}
	hd.frame = f
	return nil
}

// parseSOS reads the scan header. Only one scan with every component is
// supported: baseline files and most others have exactly that.
func (hd *header) parseSOS(data []byte, baseline bool) error {
	f := hd.frame
	if f == nil {
		return malformed("scan before the frame header")
	}
	if len(data) < 1 {
		return malformed("truncated SOS segment")
	}
	n := int(data[0])
	if len(data) != 4+2*n {
		return malformed("SOS segment of %d bytes for %d components", len(data), n)
	}
	if n != len(f.comps) {
		return unsupported("image split over several scans")
	}
	for i := range f.comps {
		c := &f.comps[i]
		if data[1+2*i] != c.id {
			return malformed("scan components differ from the frame")
		}
		c.td, c.ta = data[2+2*i]>>4, data[2+2*i]&15
		limit := byte(3)
		if baseline {
			limit = 1
		}
		if c.td > limit || c.ta > limit {
			return malformed("Huffman table %d/%d", c.td, c.ta)
		}
		if hd.huff[0][c.td] == nil || hd.huff[1][c.ta] == nil {
			return malformed("undefined Huffman table")
		}
		if hd.quant[c.tq] == nil {
			return malformed("undefined quantization table %d", c.tq)
		}
	}
	if s := data[1+2*n:]; s[0] != 0 || s[1] != 63 || s[2] != 0 {
		return malformed("spectral selection %d-%d, approximation 0x%02x in a sequential scan", s[0], s[1], s[2])
	}
	return nil
}

// unused counts table definitions the scan doesn't use: replaced ones and
// those no component refers to. Decoders never read them, so they can carry
// anything.
func (hd *header) unused() (quant, huff int) {
	usedQ := map[byte]bool{}
	usedH := [2]map[byte]bool{{}, {}}
	for _, c := range hd.frame.comps {
		usedQ[c.tq] = true
		usedH[0][c.td], usedH[1][c.ta] = true, true
	}
	for i := range byte(4) {
		quant += hd.quantDefs[i]
		if usedQ[i] {
			quant--
		}
		for tc := range 2 {
			huff += hd.huffDefs[tc][i]
			if usedH[tc][i] {
				huff--
			}
		}
	}
	return quant, huff
}

// colors decides whether the Adobe segment is kept. Decoders (libjpeg, Go)
// use its transform only for three and four components, and for three only
// when there is no JFIF segment, which implies YCbCr.
func (hd *header) colors() (unusedAdobe bool, err error) {
	if hd.adobe == nil {
		return false, nil
	}
	t := *hd.adobe
	switch n := len(hd.frame.comps); {
	case n == 1, n == 3 && hd.jfif != nil:
		return true, nil
	case n == 3 && t > 1, n == 4 && t == 1:
		return false, malformed("Adobe transform %d for %d components", t, n)
	}
	hd.keepAdobe = true
	return false, nil
}

// parseJFIF reads a JFIF APP0 segment (at least jfifLen bytes). It returns
// the size of the embedded thumbnail and of any bytes after it.
func parseJFIF(data []byte) (j *jfif, tw, th, extra int, err error) {
	j = &jfif{
		units: data[7],
		xd:    binary.BigEndian.Uint16(data[8:]),
		yd:    binary.BigEndian.Uint16(data[10:]),
	}
	if j.units > 2 {
		return nil, 0, 0, 0, malformed("JFIF density units %d", j.units)
	}
	tw, th = int(data[12]), int(data[13])
	extra = len(data) - jfifLen - 3*tw*th
	if extra < 0 {
		return nil, 0, 0, 0, malformed("truncated JFIF thumbnail")
	}
	return j, tw, th, extra, nil
}

// orientationSegment returns the APP1 data stripped files carry for
// orientation o: a minimal EXIF block holding only that tag.
func orientationSegment(o int) []byte {
	return append([]byte(prefixEXIF), exif.OrientationOnly(uint16(o))...) //nolint:gosec // o is 2 to 8
}

// exifOrientation returns the Orientation tag of an EXIF APP1 segment.
func exifOrientation(data []byte) int {
	return exif.Orientation(data[len(prefixEXIF):])
}

// isOrientationSegment reports whether data is exactly what Strip writes for
// some orientation: then it carries nothing else.
func isOrientationSegment(data []byte, o int) bool {
	return o >= 2 && bytes.Equal(data, orientationSegment(o))
}

// write writes the rebuilt headers of a stripped file, everything from SOI
// to the scan header. Every part is built from parsed values.
func (hd *header) write(w io.Writer) error {
	f := hd.frame
	b := []byte{0xff, markerSOI}

	if j := hd.jfif; j != nil {
		data := []byte(prefixJFIF)
		data = append(data, 1, 1, j.units) // version 1.01
		data = binary.BigEndian.AppendUint16(data, j.xd)
		data = binary.BigEndian.AppendUint16(data, j.yd)
		data = append(data, 0, 0) // no thumbnail
		b = appendSegment(b, markerAPP0, data)
	}
	if hd.orientation >= 2 {
		b = appendSegment(b, markerAPP0+1, orientationSegment(hd.orientation))
	}
	if hd.keepAdobe {
		// Version 100 and no flags, as libjpeg writes it.
		data := append([]byte(prefixAdobe), 0, 100, 0, 0, 0, 0, *hd.adobe)
		b = appendSegment(b, markerAPP0+14, data)
	}

	// Quantization tables, numbered in order of first use. Identical
	// tables are written once, so the choice of slots carries nothing.
	var dqt []byte
	slot := map[byte]byte{}
	ids := map[[64]uint16]byte{}
	sof := byte(markerSOF0)
	for _, c := range f.comps {
		t := hd.quant[c.tq]
		if id, ok := ids[*t]; ok {
			slot[c.tq] = id
			continue
		}
		id := byte(len(ids)) //nolint:gosec // at most 4 tables
		ids[*t], slot[c.tq] = id, id
		if slices.Max(t[:]) <= 255 {
			dqt = append(dqt, id)
			for _, v := range t {
				dqt = append(dqt, byte(v)) //nolint:gosec // checked <= 255 above
			}
			continue
		}
		// Values above 255 need 16-bit tables, which baseline doesn't allow.
		sof = markerSOF1
		dqt = append(dqt, 1<<4|id)
		for _, v := range t {
			dqt = binary.BigEndian.AppendUint16(dqt, v)
		}
	}
	b = appendSegment(b, markerDQT, dqt)

	compIDs := hd.componentIDs()
	data := []byte{8}
	data = binary.BigEndian.AppendUint16(data, uint16(f.height)) //nolint:gosec // read from 16 bits
	data = binary.BigEndian.AppendUint16(data, uint16(f.width))  //nolint:gosec // read from 16 bits
	data = append(data, byte(len(f.comps)))                      //nolint:gosec // 1 to 4 components
	for i, c := range f.comps {
		sampling := byte(c.h<<4 | c.v) //nolint:gosec // factors are 1 to 4
		if len(f.comps) == 1 {
			// A single component is never interleaved, so its factors
			// don't matter.
			sampling = 0x11
		}
		data = append(data, compIDs[i], sampling, slot[c.tq])
	}
	b = appendSegment(b, sof, data)

	dht := appendDHT(nil, 0, 0, stdLumaDC)
	dht = appendDHT(dht, 1, 0, stdLumaAC)
	if len(f.comps) > 1 {
		dht = appendDHT(dht, 0, 1, stdChromaDC)
		dht = appendDHT(dht, 1, 1, stdChromaAC)
	}
	b = appendSegment(b, markerDHT, dht)

	data = []byte{byte(len(f.comps))} //nolint:gosec // 1 to 4 components
	for i := range f.comps {
		tables := byte(0x11)
		if i == 0 {
			tables = 0x00
		}
		data = append(data, compIDs[i], tables)
	}
	b = appendSegment(b, markerSOS, append(data, 0, 63, 0))

	_, err := w.Write(b)
	return err
}

// componentIDs returns the component IDs to write: "R", "G", "B" for an RGB
// image, as libjpeg writes them, and 1, 2, ... otherwise. An image is RGB if
// a kept Adobe segment says so, or, without JFIF and Adobe segments, if its
// IDs are "R", "G", "B". Some decoders (IJG libjpeg 9) look only at the IDs.
func (hd *header) componentIDs() []byte {
	f := hd.frame
	c := f.comps
	if len(c) == 3 && hd.jfif == nil {
		rgbIDs := c[0].id == 'R' && c[1].id == 'G' && c[2].id == 'B'
		if hd.keepAdobe && *hd.adobe == 0 || !hd.keepAdobe && rgbIDs {
			return []byte("RGB")
		}
	}
	var ids []byte
	for i := range c {
		ids = append(ids, byte(i+1))
	}
	return ids
}

func ceilDiv(a, b int) int {
	return (a + b - 1) / b
}

// mcus returns the layout of the scan in MCUs. A single component is coded
// block by block; several are interleaved in MCUs of hmax x vmax blocks.
func (f *frame) mcus() (across, down int) {
	if len(f.comps) == 1 {
		return f.comps[0].bw, f.comps[0].bh
	}
	return ceilDiv(f.width, 8*f.hmax), ceilDiv(f.height, 8*f.vmax)
}
