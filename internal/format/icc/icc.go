// Package icc recognizes the color space an ICC profile describes and
// writes canonical profiles for it.
//
// Profiles carry descriptions, copyright strings, creation dates, device
// makers and models, and private tags, so they can't be kept as they are.
// Dropping them changes colors, though: phones and Macs tag photos and
// screenshots as Display P3, and a viewer that reads them as sRGB shows them
// duller. So a profile whose colors are a known space is replaced by our own
// profile for that space, built from fixed values; everything else in it is
// lost, and which space it was (a choice of three) is all that survives.
//
// Only matrix/TRC RGB profiles are recognized, by their colorants and tone
// curves. Profiles with lookup tables (A2B0 and the like) are not: color
// management uses the tables first, and they can describe anything.
package icc

import (
	"crypto/md5" //nolint:gosec // the ICC profile ID is defined as MD5
	"encoding/binary"
	"math"
	"unicode/utf16"
)

// Space is a color space a profile can describe.
type Space int

// The color spaces Identify recognizes, and Unknown for every other.
const (
	Unknown Space = iota
	SRGB          // no profile needed: viewers assume it
	DisplayP3
	AdobeRGB
)

func (s Space) String() string {
	switch s {
	case SRGB:
		return "sRGB"
	case DisplayP3:
		return "Display P3"
	case AdobeRGB:
		return "Adobe RGB (1998)"
	}
	return "unknown"
}

// MaxSize caps the profiles Identify reads. Matrix profiles take a few KB,
// and the largest tone curves 3 × 8 KB; anything bigger holds lookup tables
// or padding.
const MaxSize = 1 << 20

// maxTags caps the tag table. Real matrix profiles have 9 to 20 tags.
const maxTags = 100

// maxCurve caps the entries of a sampled tone curve (curv). Real ones have
// 1024 or 4096.
const maxCurve = 4096

const headerLen = 128

// colorants are the red, green and blue columns of a space's matrix in
// s15Fixed16, adapted to the D50 white of the profile connection space with
// the Bradford transform (computed from the primaries and the D65 white;
// they match Apple's and Adobe's profiles). They are integers, not computed
// here, because Go may fuse floating-point operations differently on each
// platform, and the profile bytes must be the same everywhere.
var colorants = map[Space][3][3]int32{
	SRGB:      {{28576, 14581, 912}, {25239, 46983, 6361}, {9375, 3972, 46787}},
	DisplayP3: {{33759, 15807, -69}, {19135, 45367, 2745}, {10296, 4363, 51385}},
	AdobeRGB:  {{39960, 20389, 1276}, {13453, 41004, 3989}, {9777, 4143, 48796}},
}

// tolerance is how far a profile's colorants may be from ours, in
// s15Fixed16 units (0.003). Vendors round and adapt differently by up to
// ~0.0002; the three spaces differ by more than 0.05.
const tolerance = 197

// curveTolerance is how far a tone curve may be from ours, as a fraction of
// full scale. A 1024-entry table is off by ~0.00002, gamma 2.2 from Adobe's
// 563/256 by ~0.0002; the sRGB curve and gamma 2.2 differ by ~0.03.
const curveTolerance = 0.002

// d50 is the profile connection space's white in s15Fixed16 (ICC.1 7.2.16).
var d50 = [3]int32{0xf6d6, 0x10000, 0xd32d}

// bradford adapts D65 to D50, in s15Fixed16, row by row: the chad tag.
var bradford = [9]int32{68674, 1502, -3291, 1939, 64912, -1119, -606, 988, 49262}

// Identify returns the color space profile p describes, or Unknown if it
// isn't one we know or can't be read.
func Identify(p []byte) Space {
	if len(p) < headerLen+4 || len(p) > MaxSize {
		return Unknown
	}
	size := binary.BigEndian.Uint32(p)
	if size < headerLen+4 || uint64(size) > uint64(len(p)) {
		return Unknown
	}
	p = p[:size]
	if string(p[36:40]) != "acsp" || string(p[16:20]) != "RGB " || string(p[20:24]) != "XYZ " {
		return Unknown
	}
	if v := p[8]; v != 2 && v != 4 {
		return Unknown
	}
	switch string(p[12:16]) {
	case "mntr", "scnr", "spac":
	default:
		return Unknown // printers, device links and the like
	}

	tags, ok := readTags(p)
	if !ok {
		return Unknown
	}
	for sig := range tags {
		switch sig[:3] {
		case "A2B", "B2A", "D2B", "B2D":
			return Unknown
		}
	}
	var cols [3][3]int32
	for i, sig := range []string{"rXYZ", "gXYZ", "bXYZ"} {
		if cols[i], ok = readXYZ(tags[sig]); !ok {
			return Unknown
		}
	}
	var curves [3]curve
	for i, sig := range []string{"rTRC", "gTRC", "bTRC"} {
		if curves[i], ok = readCurve(tags[sig]); !ok {
			return Unknown
		}
	}

	for _, s := range []Space{SRGB, DisplayP3, AdobeRGB} {
		if near(cols, colorants[s]) && sameCurves(curves, s) {
			return s
		}
	}
	return Unknown
}

// readTags reads the tag table. Every tag must lie inside the profile, and
// no signature may repeat: readers would disagree on which one counts.
func readTags(p []byte) (map[string][]byte, bool) {
	n := binary.BigEndian.Uint32(p[headerLen:])
	if n > maxTags || headerLen+4+12*int(n) > len(p) {
		return nil, false
	}
	tags := make(map[string][]byte, n)
	for i := range int(n) {
		e := p[headerLen+4+12*i:]
		sig := string(e[:4])
		off, size := binary.BigEndian.Uint32(e[4:]), binary.BigEndian.Uint32(e[8:])
		if uint64(off)+uint64(size) > uint64(len(p)) {
			return nil, false
		}
		if _, dup := tags[sig]; dup {
			return nil, false
		}
		tags[sig] = p[off : off+size]
	}
	return tags, true
}

// readXYZ reads an XYZType tag holding one value.
func readXYZ(b []byte) ([3]int32, bool) {
	if len(b) < 20 || string(b[:4]) != "XYZ " {
		return [3]int32{}, false
	}
	var v [3]int32
	for i := range v {
		v[i] = int32(binary.BigEndian.Uint32(b[8+4*i:])) //nolint:gosec // s15Fixed16 is signed
	}
	return v, true
}

func near(a, b [3][3]int32) bool {
	for i := range a {
		for j := range a[i] {
			if d := int64(a[i][j]) - int64(b[i][j]); d < -tolerance || d > tolerance {
				return false
			}
		}
	}
	return true
}

// curve is a tone curve read from a curv or para tag.
type curve struct {
	table []byte    // curv with 2 or more entries, big-endian
	gamma float64   // curv with one entry, or para type 0
	param []float64 // para: g, a, b, c, d, e, f as present
	kind  int       // para function type, -1 for curv
}

func readCurve(b []byte) (curve, bool) {
	if len(b) < 12 {
		return curve{}, false
	}
	switch string(b[:4]) {
	case "curv":
		n := binary.BigEndian.Uint32(b[8:])
		if n > maxCurve || 12+2*int(n) > len(b) {
			return curve{}, false
		}
		c := curve{kind: -1}
		switch n {
		case 0:
			c.gamma = 1
		case 1:
			c.gamma = float64(binary.BigEndian.Uint16(b[12:])) / 256
		default:
			c.table = b[12 : 12+2*n]
		}
		return c, true
	case "para":
		kind := int(binary.BigEndian.Uint16(b[8:]))
		counts := []int{1, 3, 4, 5, 7}
		if kind >= len(counts) || 12+4*counts[kind] > len(b) {
			return curve{}, false
		}
		c := curve{kind: kind, param: make([]float64, counts[kind])}
		for i := range c.param {
			c.param[i] = float64(int32(binary.BigEndian.Uint32(b[12+4*i:]))) / 65536 //nolint:gosec // s15Fixed16 is signed
		}
		return c, true
	}
	return curve{}, false
}

// at evaluates the curve at x in [0, 1] (ICC.1 10.6 and 10.18).
func (c curve) at(x float64) float64 {
	if c.kind < 0 {
		if c.table == nil {
			return math.Pow(x, c.gamma)
		}
		n := len(c.table) / 2
		pos := x * float64(n-1)
		i := min(int(pos), n-2)
		f := pos - float64(i)
		lo, hi := binary.BigEndian.Uint16(c.table[2*i:]), binary.BigEndian.Uint16(c.table[2*i+2:])
		return (float64(lo)*(1-f) + float64(hi)*f) / 65535
	}
	p := c.param
	g := p[0]
	switch c.kind {
	case 0:
		return math.Pow(x, g)
	case 1:
		if x >= -p[2]/p[1] {
			return math.Pow(p[1]*x+p[2], g)
		}
		return 0
	case 2:
		if x >= -p[2]/p[1] {
			return math.Pow(p[1]*x+p[2], g) + p[3]
		}
		return p[3]
	case 3:
		if x >= p[4] {
			return math.Pow(p[1]*x+p[2], g)
		}
		return p[3] * x
	default:
		if x >= p[4] {
			return math.Pow(p[1]*x+p[2], g) + p[5]
		}
		return p[3]*x + p[6]
	}
}

// curvePoints is how many points of a tone curve are compared.
const curvePoints = 64

// sameCurves reports whether all three curves are space s's.
func sameCurves(cs [3]curve, s Space) bool {
	for _, c := range cs {
		for i := range curvePoints + 1 {
			x := float64(i) / curvePoints
			// NaN fails the comparison too.
			if !(math.Abs(c.at(x)-reference(s, x)) <= curveTolerance) {
				return false
			}
		}
	}
	return true
}

// reference is space s's tone curve: the sRGB curve for sRGB and Display
// P3, gamma 563/256 for Adobe RGB.
func reference(s Space, x float64) float64 {
	if s == AdobeRGB {
		return math.Pow(x, 563.0/256)
	}
	if x < 0.04045 {
		return x / 12.92
	}
	return math.Pow((x+0.055)/1.055, 2.4)
}

// Profile returns our profile for s, Display P3 or Adobe RGB, or nil for
// other spaces (sRGB needs none). The slice is shared: don't modify it.
func Profile(s Space) []byte {
	return profiles[s]
}

var profiles = map[Space][]byte{
	DisplayP3: build(DisplayP3),
	AdobeRGB:  build(AdobeRGB),
}

// The tone curves as para tags, in s15Fixed16: the sRGB curve (function
// type 3: g, a, b, c, d) and gamma 563/256 (type 0), like Apple's and
// Adobe's profiles.
var (
	srgbCurve  = []int32{157286, 62119, 3417, 5072, 2651}
	adobeCurve = []int32{144128}
)

// build writes an ICC v4.3 display profile with the fewest tags color
// management needs: description, copyright, white point, colorants, tone
// curves (one tag shared by all three) and the chromatic adaptation.
func build(s Space) []byte {
	type tag struct {
		sig  string
		data []byte
	}
	trc := paraTag(srgbCurve)
	if s == AdobeRGB {
		trc = paraTag(adobeCurve)
	}
	c := colorants[s]
	tags := []tag{
		{"desc", mlucTag(s.String())},
		{"cprt", mlucTag("No copyright, use freely")},
		{"wtpt", xyzTag(d50)},
		{"rXYZ", xyzTag(c[0])},
		{"gXYZ", xyzTag(c[1])},
		{"bXYZ", xyzTag(c[2])},
		{"rTRC", trc},
		{"gTRC", trc},
		{"bTRC", trc},
		{"chad", sf32Tag(bradford[:])},
	}

	be := binary.BigEndian
	table := be.AppendUint32(nil, uint32(len(tags))) //nolint:gosec // ten tags
	var data []byte
	offset := map[*byte]uint32{} // shared data is written once
	start := headerLen + 4 + 12*len(tags)
	for _, t := range tags {
		off, ok := offset[&t.data[0]]
		if !ok {
			off = uint32(start + len(data)) //nolint:gosec // a few hundred bytes
			offset[&t.data[0]] = off
			data = append(data, t.data...)
			for len(data)%4 != 0 {
				data = append(data, 0)
			}
		}
		table = append(table, t.sig...)
		table = be.AppendUint32(table, off)
		table = be.AppendUint32(table, uint32(len(t.data))) //nolint:gosec // a few dozen bytes
	}

	size := start + len(data)
	p := make([]byte, headerLen, size)
	be.PutUint32(p[0:], uint32(size)) //nolint:gosec // a few hundred bytes
	be.PutUint32(p[8:], 0x04300000)   // version 4.3
	copy(p[12:], "mntrRGB XYZ ")      // display, RGB data, XYZ connection space
	be.PutUint16(p[24:], 2000)        // created 2000-01-01, a fixed date
	be.PutUint16(p[26:], 1)
	be.PutUint16(p[28:], 1)
	copy(p[36:], "acsp")
	for i, v := range d50 {
		be.PutUint32(p[68+4*i:], uint32(v)) //nolint:gosec // positive
	}
	p = append(append(p, table...), data...)
	// The profile ID is the MD5 of the profile with the flags, rendering
	// intent and ID zeroed (ICC.1 7.2.18); ours are zero already.
	id := md5.Sum(p) //nolint:gosec // the ICC profile ID is defined as MD5
	copy(p[84:], id[:])
	return p
}

func xyzTag(v [3]int32) []byte {
	b := []byte("XYZ \x00\x00\x00\x00")
	for _, x := range v {
		b = binary.BigEndian.AppendUint32(b, uint32(x)) //nolint:gosec // s15Fixed16 is signed
	}
	return b
}

func sf32Tag(v []int32) []byte {
	b := []byte("sf32\x00\x00\x00\x00")
	for _, x := range v {
		b = binary.BigEndian.AppendUint32(b, uint32(x)) //nolint:gosec // s15Fixed16 is signed
	}
	return b
}

func paraTag(params []int32) []byte {
	kind := map[int]uint16{1: 0, 5: 3}[len(params)]
	b := []byte("para\x00\x00\x00\x00")
	b = binary.BigEndian.AppendUint16(b, kind)
	b = append(b, 0, 0)
	for _, x := range params {
		b = binary.BigEndian.AppendUint32(b, uint32(x)) //nolint:gosec // s15Fixed16 is signed
	}
	return b
}

// mlucTag returns a multiLocalizedUnicodeType holding s in English.
func mlucTag(s string) []byte {
	text := utf16.Encode([]rune(s))
	b := []byte("mluc\x00\x00\x00\x00")
	b = binary.BigEndian.AppendUint32(b, 1)  // one record
	b = binary.BigEndian.AppendUint32(b, 12) // of 12 bytes
	b = append(b, "enUS"...)
	b = binary.BigEndian.AppendUint32(b, uint32(2*len(text))) //nolint:gosec // a short constant
	b = binary.BigEndian.AppendUint32(b, 28)                  // offset of the text
	for _, u := range text {
		b = binary.BigEndian.AppendUint16(b, u)
	}
	return b
}
