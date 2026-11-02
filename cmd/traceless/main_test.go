package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestInspectJSON(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.png")
	if err := os.WriteFile(broken, []byte("\x89PNG\r\n\x1a\ntruncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join("..", "..", "internal", "format", "png", "testdata", "metadata.png")

	results, code := inspectAll(context.Background(), []string{fixture, broken})
	if code != exitFail {
		t.Errorf("exit code %d, want %d", code, exitFail)
	}
	out, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		File   string            `json:"file"`
		Format string            `json:"format"`
		Fields []json.RawMessage `json:"fields"`
		Error  string            `json:"error"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	if r := got[0]; r.Format != "PNG" || len(r.Fields) == 0 || r.Error != "" {
		t.Errorf("fixture result = %+v", r)
	}
	if !strings.Contains(string(out), `{"name":"EXIF GPS position","value":"52.520000, 13.405000"}`) {
		t.Error("GPS position missing from JSON output")
	}
	if r := got[1]; r.Format != "PNG" || r.Fields == nil || len(r.Fields) != 0 || !strings.Contains(r.Error, "malformed") {
		t.Errorf("broken result = %+v", r)
	}
}
