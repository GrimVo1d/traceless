package icc

import (
	"testing"

	"traceless/internal/format/icc/icctest"
)

// FuzzIdentify feeds arbitrary profiles to Identify. Nothing may panic.
func FuzzIdentify(f *testing.F) {
	f.Add(Profile(DisplayP3))
	f.Add(Profile(AdobeRGB))
	f.Add(icctest.Build(icctest.Profile{Version: 2, Colorants: icctest.SRGB, Curve: icctest.SRGBTable}))
	f.Add(icctest.Build(icctest.Profile{Version: 2, Colorants: icctest.AdobeRGB, Curve: icctest.Gamma22}))
	f.Fuzz(func(_ *testing.T, p []byte) {
		Identify(p)
	})
}
