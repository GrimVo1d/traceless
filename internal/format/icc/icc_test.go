package icc

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"

	"traceless/internal/format/icc/icctest"
)

func TestIdentify(t *testing.T) {
	secret := map[string][]byte{"dmnd": icctest.Text("Secret Corp"), "dmdd": icctest.Text("Pixel 7 Pro")}
	for _, tt := range []struct {
		name string
		p    icctest.Profile
		want Space
	}{
		{"sRGB table, v2", icctest.Profile{Version: 2, Colorants: icctest.SRGB, Curve: icctest.SRGBTable, Tags: secret}, SRGB},
		{"sRGB para, v4", icctest.Profile{Version: 4, Colorants: icctest.SRGB, Curve: icctest.SRGBPara}, SRGB},
		{"Display P3", icctest.Profile{Version: 4, Colorants: icctest.DisplayP3, Curve: icctest.SRGBPara, Tags: secret}, DisplayP3},
		{"Display P3, table", icctest.Profile{Version: 2, Colorants: icctest.DisplayP3, Curve: icctest.SRGBTable}, DisplayP3},
		{"Display P3, input class", icctest.Profile{Version: 4, Class: "scnr", Colorants: icctest.DisplayP3, Curve: icctest.SRGBPara}, DisplayP3},
		{"Adobe RGB", icctest.Profile{Version: 2, Colorants: icctest.AdobeRGB, Curve: icctest.Gamma22, Tags: secret}, AdobeRGB},
		{"DCI-P3 white", icctest.Profile{Version: 4, Colorants: icctest.DCIP3, Curve: icctest.SRGBPara}, Unknown},
		{"P3 with gamma 1.8", icctest.Profile{Version: 4, Colorants: icctest.DisplayP3, Curve: icctest.Gamma18}, Unknown},
		{"Adobe primaries, sRGB curve", icctest.Profile{Version: 2, Colorants: icctest.AdobeRGB, Curve: icctest.SRGBPara}, Unknown},
		{"version 5", icctest.Profile{Version: 5, Colorants: icctest.DisplayP3, Curve: icctest.SRGBPara}, Unknown},
		{"printer class", icctest.Profile{Version: 4, Class: "prtr", Colorants: icctest.DisplayP3, Curve: icctest.SRGBPara}, Unknown},
		{"lookup table", icctest.Profile{Version: 4, Colorants: icctest.DisplayP3, Curve: icctest.SRGBPara, Tags: map[string][]byte{"A2B0": icctest.Text("x")}}, Unknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Identify(icctest.Build(tt.p)); got != tt.want {
				t.Errorf("Identify = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIdentifyBroken(t *testing.T) {
	p3 := icctest.Build(icctest.Profile{Version: 4, Colorants: icctest.DisplayP3, Curve: icctest.SRGBPara})
	put32 := func(off int, v uint32) []byte {
		b := bytes.Clone(p3)
		binary.BigEndian.PutUint32(b[off:], v)
		return b
	}
	tagOff := func(sig string) int {
		for i := range int(binary.BigEndian.Uint32(p3[128:])) {
			if string(p3[132+12*i:][:4]) == sig {
				return 132 + 12*i
			}
		}
		t.Fatalf("no %s tag", sig)
		return 0
	}
	dup := bytes.Clone(p3)
	copy(dup[tagOff("gXYZ"):], "rXYZ")
	for name, b := range map[string][]byte{
		"empty":              nil,
		"header only":        p3[:128],
		"truncated":          p3[:len(p3)-1],
		"size too small":     put32(0, 100),
		"no signature":       put32(36, 0),
		"too many tags":      put32(128, maxTags+1),
		"tag outside":        put32(tagOff("rXYZ")+4, uint32(len(p3))),
		"tag size overflow":  put32(tagOff("rXYZ")+8, 0xffffffff),
		"duplicate tag":      dup,
		"short XYZ":          put32(tagOff("rXYZ")+8, 12),
		"unknown curve type": put32(int(binary.BigEndian.Uint32(p3[tagOff("rTRC")+4:])), 0),
	} {
		if got := Identify(b); got != Unknown {
			t.Errorf("%s: Identify = %v, want unknown", name, got)
		}
	}
}

// Our profiles identify as their own space, and nothing in them comes from
// an input: they are the same bytes every time.
func TestProfile(t *testing.T) {
	for _, s := range []Space{DisplayP3, AdobeRGB} {
		p := Profile(s)
		if got := Identify(p); got != s {
			t.Errorf("Identify(Profile(%v)) = %v", s, got)
		}
		if int(binary.BigEndian.Uint32(p)) != len(p) {
			t.Errorf("%v: size field %d, profile %d bytes", s, binary.BigEndian.Uint32(p), len(p))
		}
		if !bytes.Equal(p, build(s)) {
			t.Errorf("%v: build isn't deterministic", s)
		}
	}
	if Profile(SRGB) != nil || Profile(Unknown) != nil {
		t.Error("Profile returns a profile for sRGB or unknown")
	}
}

var update = os.Getenv("ICC_UPDATE") != ""

// The profiles are part of every stripped file, so a change breaks Verify of
// files stripped before it. testdata holds them for checking with other
// tools (run with ICC_UPDATE=1 to rewrite).
func TestProfileGolden(t *testing.T) {
	for _, s := range []Space{DisplayP3, AdobeRGB} {
		path := "testdata/" + map[Space]string{DisplayP3: "display-p3.icc", AdobeRGB: "adobe-rgb.icc"}[s]
		if update {
			if err := os.WriteFile(path, Profile(s), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with ICC_UPDATE=1)", err)
		}
		if !bytes.Equal(Profile(s), want) {
			t.Errorf("%v profile differs from %s", s, path)
		}
	}
}

// Profiles that come with macOS, when there are any: real profiles from
// another vendor.
func TestSystemProfiles(t *testing.T) {
	const dir = "/System/Library/ColorSync/Profiles/"
	for name, want := range map[string]Space{
		"Display P3.icc":           DisplayP3,
		"sRGB Profile.icc":         SRGB,
		"AdobeRGB1998.icc":         AdobeRGB,
		"DCI(P3) RGB.icc":          Unknown,
		"ITU-709.icc":              Unknown,
		"ROMM RGB.icc":             Unknown,
		"Generic Gray Profile.icc": Unknown,
	} {
		p, err := os.ReadFile(dir + name)
		if err != nil {
			t.Skip("no system profiles")
		}
		if got := Identify(p); got != want {
			t.Errorf("%s: Identify = %v, want %v", name, got, want)
		}
	}
}
