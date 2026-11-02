// Package core is the entry point every frontend (CLI, HTTP API, WASM UI)
// uses. It picks the handler for an input and wraps stripping with
// verification.
package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"traceless/internal/format"
	"traceless/internal/format/jpeg"
	"traceless/internal/format/png"
)

// handlers lists every supported format, tried in order during detection.
var handlers = []format.Handler{
	png.Handler{},
	jpeg.Handler{},
}

// ErrHardLinked means an in-place strip was refused because other hard links
// to the file would keep pointing at the original data.
var ErrHardLinked = errors.New("file has other hard links that would keep the original data")

// InspectFile detects the format of the file at path and lists its metadata.
func InspectFile(ctx context.Context, path string) (format.Handler, []format.Field, error) {
	f, _, err := openRegular(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	br := bufio.NewReader(ctxReader{ctx, f})
	h, err := format.Detect(br, handlers)
	if err != nil {
		return nil, nil, err
	}
	fields, err := h.Inspect(br)
	return h, fields, err
}

// VerifyFile checks that the file at path contains no metadata.
func VerifyFile(ctx context.Context, path string) (format.Handler, error) {
	f, _, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	br := bufio.NewReader(ctxReader{ctx, f})
	h, err := format.Detect(br, handlers)
	if err != nil {
		return nil, err
	}
	return h, h.Verify(br)
}

// StripFile writes a copy of src without metadata to a new file dst, created
// with mode 0600. It never overwrites an existing dst.
//
// Output goes to a temporary file next to dst, is verified, and only then
// moved into place, so dst never holds partial or unverified output.
// Canceling ctx stops the strip and removes the temporary file.
func StripFile(ctx context.Context, src, dst string) (format.Handler, error) {
	return stripFile(ctx, src, dst, false)
}

// StripInPlace replaces path with a copy without metadata, the same way
// StripFile writes a new file. The file keeps its permission bits and must
// have no other hard links, which would keep the original data reachable.
func StripInPlace(ctx context.Context, path string) (format.Handler, error) {
	return stripFile(ctx, path, path, true)
}

func stripFile(ctx context.Context, src, dst string, inPlace bool) (format.Handler, error) {
	in, info, err := openRegular(src)
	if err != nil {
		return nil, err
	}
	defer in.Close()

	perm := os.FileMode(0o600)
	if inPlace {
		// Use the file we opened rather than looking the path up again.
		if linkCount(info) > 1 {
			return nil, fmt.Errorf("%s: %w", dst, ErrHardLinked)
		}
		perm = info.Mode().Perm()
	}

	br := bufio.NewReader(ctxReader{ctx, in})
	h, err := format.Detect(br, handlers)
	if err != nil {
		return nil, err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".traceless-*")
	if err != nil {
		return h, err
	}
	keepTmp := false
	defer func() {
		if !keepTmp {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	bw := bufio.NewWriter(ctxWriter{ctx, tmp})
	if err := h.Strip(br, bw); err != nil {
		return h, err
	}
	if err := bw.Flush(); err != nil {
		return h, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return h, err
	}
	if err := h.Verify(bufio.NewReader(ctxReader{ctx, tmp})); err != nil {
		return h, fmt.Errorf("verifying output: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		return h, err
	}
	if err := tmp.Sync(); err != nil {
		return h, err
	}
	if err := tmp.Close(); err != nil {
		return h, err
	}
	if err := ctx.Err(); err != nil {
		return h, err
	}

	if inPlace {
		if err := os.Rename(tmp.Name(), dst); err != nil {
			return h, err
		}
		keepTmp = true
		return h, nil
	}
	renamed, err := publish(tmp.Name(), dst)
	if renamed {
		keepTmp = true
	}
	return h, err
}

// link is os.Link, replaced in tests.
var link = os.Link

// publish moves tmp to dst without ever replacing an existing dst. A hard
// link fails atomically if dst exists; the caller then removes tmp.
// Filesystems without hard links (FAT, exFAT) fall back to check-then-rename,
// which can lose a race, and report renamed so tmp is left alone.
func publish(tmp, dst string) (renamed bool, err error) {
	err = link(tmp, dst)
	if errors.Is(err, fs.ErrExist) {
		return false, fmt.Errorf("%s: %w", dst, fs.ErrExist)
	}
	if err == nil {
		return false, nil
	}
	if _, statErr := os.Lstat(dst); statErr == nil {
		return false, fmt.Errorf("%s: %w", dst, fs.ErrExist)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return false, err
	}
	return true, nil
}

// ctxReader fails reads once ctx is canceled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr ctxReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}

// ctxWriter fails writes once ctx is canceled.
type ctxWriter struct {
	ctx context.Context
	w   io.Writer
}

func (cw ctxWriter) Write(p []byte) (int, error) {
	if err := cw.ctx.Err(); err != nil {
		return 0, err
	}
	return cw.w.Write(p)
}
