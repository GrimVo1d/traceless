//go:build !unix

package core

import (
	"fmt"
	"os"
)

// openRegular opens path for reading if it is a regular file.
func openRegular(path string) (*os.File, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%s: not a regular file", path)
	}
	return f, info, nil
}

// linkCount returns the number of hard links to a file. Without a portable
// way to read it, assume one.
func linkCount(os.FileInfo) uint64 {
	return 1
}
