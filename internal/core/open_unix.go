//go:build unix

package core

import (
	"fmt"
	"os"
	"syscall"
)

// openRegular opens path for reading if it is a regular file. It opens
// without blocking and checks the type on the open descriptor, so a FIFO or
// device swapped in after a check can't hang the tool or feed it endless data.
func openRegular(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
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

// linkCount returns the number of hard links to a file.
func linkCount(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}
