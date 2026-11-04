// Package icctest builds ICC profiles for tests, shaped like the ones
// cameras, phones and editors embed.
package icctest

import (
	"encoding/binary"
	"math"
)

// Colorants of the spaces, as vendors write them (Apple's and HP's
// profiles): close to, but not always equal to, the canonical ones.
var (
	SRGB      = [3][3]int32{{28578, 14581, 912}, {25241, 46981, 6362}, {9376, 3972, 46799}}
	DisplayP3 = [3][3]int32{{33759, 15807, -69}, {19135, 45367, 2745}, {10296, 4363, 51385}}
	AdobeRGB  = [3][3]int32{{39960, 20389, 1276}, {13453, 41004, 3989}, {9777, 4143, 48796}}
	// DCIP3 is P3 with the DCI white point, which isn't recognized.
	DCIP3 = [3][3]int32{{31861, 14856, -53}, {21224, 46552, 2833}, {10105, 4128, 51280}}
)

// Tone curves, as tag data.
var (
	// SRGBTable is the sRGB curve sampled at 1024 points, as in HP's profile.
	SRGBTable = curvTable(1024, func(x float64) float64 {
		if x < 0.04045 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	})
	// SRGBPara is the sRGB curve as a parametric function, as in Apple's.
	SRGBPara = para(3, 157286, 62119, 3417, 5072, 2651)
	// Gamma22 is gamma 563/256 as one curv entry, as in Adobe's.
	Gamma22 = []byte{'c', 'u', 'r', 'v', 0, 0, 0, 0, 0, 0, 0, 1, 0x02, 0x33}
	// Gamma18 is gamma 1.8, which no recognized space uses.
	Gamma18 = para(0, 117965)
)

// Profile describes a profile to build.
type Profile struct {
	Version   byte // major version, 2 or 4
	Class     string
	Colorants [3][3]int32
	Curve     []byte            // tag data shared by rTRC, gTRC and bTRC
	Tags      map[string][]byte // more tags, e.g. Text("secret") under "dmnd"
}

// Build writes the profile. Its header names a device maker and model and a
// creation date, and it describes itself with Text tags.
func Build(p Profile) []byte {
	if p.Class == "" {
		p.Class = "mntr"
	}
	type tag struct {
		sig  string
		data []byte
	}
	tags := []tag{
		{"desc", Text("Camera profile by Secret Corp")},
		{"cprt", Text("Copyright 2024 Secret Corp")},
		{"wtpt", xyz([3]int32{62289, 65536, 71372})},
	}
	for i, sig := range []string{"rXYZ", "gXYZ", "bXYZ"} {
		tags = append(tags, tag{sig, xyz(p.Colorants[i])})
	}
	for _, sig := range []string{"rTRC", "gTRC", "bTRC"} {
		tags = append(tags, tag{sig, p.Curve})
	}
	for _, sig := range sortedKeys(p.Tags) {
		tags = append(tags, tag{sig, p.Tags[sig]})
	}

	be := binary.BigEndian
	start := 128 + 4 + 12*len(tags)
	table := be.AppendUint32(nil, uint32(len(tags))) //nolint:gosec // a few tags
	var data []byte
	for _, t := range tags {
		table = append(table, t.sig...)
		table = be.AppendUint32(table, uint32(start+len(data))) //nolint:gosec // small
		table = be.AppendUint32(table, uint32(len(t.data)))     //nolint:gosec // small
		data = append(data, t.data...)
		for len(data)%4 != 0 {
			data = append(data, 0)
		}
	}
	h := make([]byte, 128)
	be.PutUint32(h, uint32(start+len(data))) //nolint:gosec // small
	copy(h[4:], "lcms")
	h[8] = p.Version
	copy(h[12:], p.Class+"RGB XYZ ")
	be.PutUint16(h[24:], 2024) // 2024-05-01 12:30:00
	be.PutUint16(h[26:], 5)
	be.PutUint16(h[28:], 1)
	be.PutUint16(h[30:], 12)
	be.PutUint16(h[32:], 30)
	copy(h[36:], "acspAPPL")
	copy(h[48:], "SCRPPX7P") // device maker and model
	copy(h[68:], []byte{0, 0, 0xf6, 0xd6, 0, 1, 0, 0, 0, 0, 0xd3, 0x2d})
	copy(h[80:], "SCRP")
	return append(append(h, table...), data...)
}

// Text returns a v2 textType tag.
func Text(s string) []byte {
	return append([]byte("text\x00\x00\x00\x00"+s), 0)
}

func xyz(v [3]int32) []byte {
	b := []byte("XYZ \x00\x00\x00\x00")
	for _, x := range v {
		b = binary.BigEndian.AppendUint32(b, uint32(x)) //nolint:gosec // signed
	}
	return b
}

func para(kind uint16, params ...int32) []byte {
	b := binary.BigEndian.AppendUint16([]byte("para\x00\x00\x00\x00"), kind)
	b = append(b, 0, 0)
	for _, x := range params {
		b = binary.BigEndian.AppendUint32(b, uint32(x)) //nolint:gosec // signed
	}
	return b
}

func curvTable(n int, f func(float64) float64) []byte {
	b := binary.BigEndian.AppendUint32([]byte("curv\x00\x00\x00\x00"), uint32(n)) //nolint:gosec // small
	for i := range n {
		b = binary.BigEndian.AppendUint16(b, uint16(math.Round(f(float64(i)/float64(n-1))*65535)))
	}
	return b
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
