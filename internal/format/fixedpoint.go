package format

import (
	"bytes"
	"errors"
	"io"
)

// maxLag caps how far the input may run ahead of the rebuilt output, or the
// other way round, in VerifyFixedPoint. A canonical file stays within a few
// encoder buffers (under 1 MB); one that drifts further is not canonical.
const maxLag = 4 << 20

// errDiffers stops a rebuild once its output differs from its input.
var errDiffers = errors.New("rebuilt file differs from the input")

// VerifyFixedPoint checks that a file is exactly what Strip writes for it.
// rebuild reads the file from in and writes the rebuilt file to out; the two
// streams are compared as they are produced, so a file that differs fails at
// the first differing byte rather than after a whole rebuild. It returns
// differs if they differ, or the error rebuild returned. A broken file can
// differ before rebuild gets to the part that would fail, so it may get
// differs where Strip reports it malformed; both fail closed.
func VerifyFixedPoint(r io.Reader, differs error, rebuild func(in io.Reader, out io.Writer) error) error {
	c := &comparison{}
	err := rebuild(io.TeeReader(r, side{c, true}), side{c, false})
	switch {
	case c.failed:
		// Whatever rebuild returned followed from the comparison failing.
		return differs
	case err != nil:
		return err
	case len(c.ahead) > 0:
		return differs
	}
	return nil
}

// comparison matches two byte streams written in any interleaving. Only the
// bytes of the side that is ahead are kept.
type comparison struct {
	ahead   []byte // unmatched bytes of one side
	inAhead bool   // they are input bytes
	failed  bool
}

func (c *comparison) write(p []byte, in bool) error {
	if c.failed {
		return errDiffers
	}
	if len(c.ahead) == 0 {
		c.inAhead = in
	}
	if in != c.inAhead {
		n := min(len(p), len(c.ahead))
		if !bytes.Equal(p[:n], c.ahead[:n]) {
			c.failed = true
			return errDiffers
		}
		c.ahead, p = c.ahead[n:], p[n:]
		if len(p) == 0 {
			return nil
		}
		c.inAhead = in
	}
	if len(c.ahead)+len(p) > maxLag {
		c.failed = true
		return errDiffers
	}
	c.ahead = append(c.ahead, p...)
	return nil
}

// side is the writer for the input or the output of a comparison.
type side struct {
	c  *comparison
	in bool
}

func (s side) Write(p []byte) (int, error) {
	if err := s.c.write(p, s.in); err != nil {
		return 0, err
	}
	return len(p), nil
}
