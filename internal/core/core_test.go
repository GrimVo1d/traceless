package core

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"traceless/internal/format"
)

const fixture = "../format/png/testdata/metadata.png"

// copyFixture copies the fixture into dir and returns its path.
func copyFixture(t *testing.T, dir string, perm os.FileMode) string {
	t.Helper()
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "photo.png")
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertOnly fails unless dir holds exactly the named entries.
func assertOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != len(names) {
		t.Fatalf("directory holds %v, want %v", got, names)
	}
	for i := range names {
		if got[i] != names[i] {
			t.Fatalf("directory holds %v, want %v", got, names)
		}
	}
}

func TestStripFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "clean.png")
	if _, err := StripFile(context.Background(), fixture, dst, false); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyFile(context.Background(), dst); err != nil {
		t.Errorf("VerifyFile: %v", err)
	}
	if _, err := VerifyFile(context.Background(), fixture); !errors.Is(err, format.ErrMetadataRemains) {
		t.Errorf("VerifyFile(context.Background(), fixture) = %v, want ErrMetadataRemains", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("new file mode %v, want 0600", info.Mode().Perm())
	}
	assertOnly(t, dir, "clean.png")
}

func TestStripFileNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "clean.png")
	if err := os.WriteFile(dst, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := StripFile(context.Background(), fixture, dst, false); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("StripFile = %v, want ErrExist", err)
	}
	if data, _ := os.ReadFile(dst); string(data) != "keep me" {
		t.Error("existing file was changed")
	}
	assertOnly(t, dir, "clean.png")
}

func TestStripFileInPlace(t *testing.T) {
	dir := t.TempDir()
	path := copyFixture(t, dir, 0o640)
	if _, err := StripFile(context.Background(), path, path, true); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyFile(context.Background(), path); err != nil {
		t.Errorf("VerifyFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640", info.Mode().Perm())
	}
	assertOnly(t, dir, "photo.png")
}

func TestStripFileRefusesHardLinks(t *testing.T) {
	dir := t.TempDir()
	path := copyFixture(t, dir, 0o644)
	if err := os.Link(path, filepath.Join(dir, "other.png")); err != nil {
		t.Skip("hard links not supported:", err)
	}
	if _, err := StripFile(context.Background(), path, path, true); !errors.Is(err, ErrHardLinked) {
		t.Errorf("StripFile = %v, want ErrHardLinked", err)
	}
}

// A failed strip must leave neither the destination nor temporary files.
func TestStripFileFailureLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "broken.png")
	if err := os.WriteFile(src, []byte("\x89PNG\r\n\x1a\ntruncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := StripFile(context.Background(), src, filepath.Join(dir, "out.png"), false); !errors.Is(err, format.ErrMalformed) {
		t.Fatalf("StripFile = %v, want ErrMalformed", err)
	}
	assertOnly(t, dir, "broken.png")
}

func TestStripFileCancelled(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := StripFile(ctx, fixture, filepath.Join(dir, "out.png"), false); !errors.Is(err, context.Canceled) {
		t.Fatalf("StripFile = %v, want context.Canceled", err)
	}
	assertOnly(t, dir)
}

func TestUnknownFormatFailsClosed(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(src, []byte("author: Jane Doe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := StripFile(context.Background(), src, filepath.Join(dir, "out.txt"), false); !errors.Is(err, format.ErrUnknownFormat) {
		t.Errorf("StripFile = %v, want ErrUnknownFormat", err)
	}
	if _, _, err := InspectFile(context.Background(), src); !errors.Is(err, format.ErrUnknownFormat) {
		t.Errorf("InspectFile = %v, want ErrUnknownFormat", err)
	}
}

func TestRefusesNonRegularFiles(t *testing.T) {
	if _, _, err := InspectFile(context.Background(), t.TempDir()); err == nil {
		t.Error("InspectFile accepted a directory")
	}
}
