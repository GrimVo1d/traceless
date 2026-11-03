//go:build unix

package core

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"traceless/internal/format"
)

// Opening a FIFO must fail fast instead of blocking until a writer appears.
func TestRefusesFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe.png")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip("mkfifo not supported:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := InspectFile(context.Background(), format.DefaultLimits, path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("InspectFile accepted a FIFO")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("InspectFile blocked on a FIFO")
	}
}
