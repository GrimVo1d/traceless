package main

import "testing"

// Non-ASCII inputs are written as UTF-8 byte escapes so invisible characters
// stay visible in the source.
func TestSafe(t *testing.T) {
	tests := map[string]string{
		"photo.png":               "photo.png",
		"caf\xc3\xa9":             "caf\xc3\xa9",
		"\x1b[2J\x1b]0;pwned":     `\u001b[2J\u001b]0;pwned`,
		"line\nbreak":             `line\u000abreak`,
		"evil\xe2\x80\xaegnp.exe": `evil\u202egnp.exe`,
		"nul\x00byte":             `nul\u0000byte`,
		"invalid \xff utf-8":      "invalid \xef\xbf\xbd utf-8",
		"csi\xc2\x9b":             `csi\u009b`,
		"filler\xe3\x85\xa4":      `filler\u3164`,
		"hangul\xe1\x85\x9f":      `hangul\u115f`,
		"vs\xef\xb8\x8f":          `vs\ufe0f`,
		"vs\xf3\xa0\x84\x80":      `vs\U000e0100`,
		"braille\xe2\xa0\x80":     `braille\u2800`,
		"cgj\xcd\x8f":             `cgj\u034f`,
		"\xcc\x81start":           `\u0301start`,
		"e\xcc\x81":               "e\xcc\x81",
		"a" + marks(5):            "a" + marks(4) + `\u0301`,
	}
	for in, want := range tests {
		if got := safe(in); got != want {
			t.Errorf("safe(%q) = %q, want %q", in, got, want)
		}
	}
}

// marks returns n combining acute accents (U+0301).
func marks(n int) string {
	s := ""
	for range n {
		s += "\xcc\x81"
	}
	return s
}
