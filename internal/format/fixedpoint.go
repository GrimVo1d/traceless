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
// differs where Strip reports it malformed; both fail closed. The bytes kept
// for the comparison are held in b.
func VerifyFixedPoint(b *Budget, r io.Reader, differs error, rebuild func(in io.Reader, out io.Writer) error) error {
	c := &comparison{ahead: NewBuffer(b)}
	defer c.ahead.Release()
	err := rebuild(io.TeeReader(r, side{c, true}), side{c, false})
	switch {
	case c.failed:
		// Whatever rebuild returned followed from the comparison failing.
		return differs
	case c.err != nil:
		// A rebuild may have swallowed it, losing the bytes it was about.
		return c.err
	case err != nil:
		return err
	case c.ahead.Len() > 0:
		return differs
	}
	return nil
}

// comparison matches two byte streams written in any interleaving. Only the
// bytes of the side that is ahead are kept.
type comparison struct {
	ahead   *Buffer // unmatched bytes of one side
	inAhead bool    // they are input bytes
	failed  bool
	err     error // budget error, sticky like failed
}

func (c *comparison) write(p []byte, in bool) error {
	if c.failed {
		return errDiffers
	}
	if c.err != nil {
		return c.err
	}
	if c.ahead.Len() == 0 {
		c.inAhead = in
	}
	if in != c.inAhead {
		n := min(len(p), c.ahead.Len())
		if !bytes.Equal(p[:n], c.ahead.Bytes()[:n]) {
			c.failed = true
			return errDiffers
		}
		c.ahead.Discard(n)
		if p = p[n:]; len(p) == 0 {
			return nil
		}
		c.inAhead = in
	}
	if c.ahead.Len()+len(p) > maxLag {
		c.failed = true
		return errDiffers
	}
	if _, err := c.ahead.Write(p); err != nil {
		// Out of budget: the error says why, so it isn't a difference.
		c.err = err
		return err
	}
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
