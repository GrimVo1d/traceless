package format

import (
	"context"
	"errors"
	"fmt"
)

// ErrLimit means the input needs more memory, work or nesting than its
// budget allows. The file may be valid: a frontend reports it as too large
// rather than malformed.
var ErrLimit = errors.New("resource limit exceeded")

// Limits bounds what one top-level call may spend, nested files included.
type Limits struct {
	Memory       int64 // bytes held at once: buffered pixels, coefficients, row buffers
	Decompressed int64 // bytes decompressed in total
	Work         int64 // units of work in total: JPEG blocks visited per scan, PNG rows
	Depth        int   // levels of files inside files below the top-level one
}

// DefaultLimits fit the largest files the handlers accept on their own:
// JPEG coefficients (256 MB) plus row buffers, a 16384x16384 RGBA16 PNG
// (2 GiB of pixels), and 100 scans over the largest buffered JPEG.
var DefaultLimits = Limits{
	Memory:       512 << 20,
	Decompressed: 4 << 30,
	Work:         1 << 30,
	Depth:        4,
}

// checkEvery is how many units of decompression or work pass between two
// checks of the context.
const checkEvery = 1 << 16

// Budget tracks what one top-level call spends against its limits. The
// handlers of nested files share their parent's budget, so the limits cover
// the whole input. It also carries the call's context: long loops that read
// no input still stop when it is canceled.
//
// A Budget is not safe for concurrent use.
type Budget struct {
	ctx    context.Context
	limits Limits

	held         int64
	decompressed int64
	work         int64
	depth        int
	sinceCheck   int64 // units since the context was last checked
}

// NewBudget returns a budget for one call. A nil ctx means
// context.Background.
func NewBudget(ctx context.Context, l Limits) *Budget {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Budget{ctx: ctx, limits: l}
}

// errNegative is returned for a negative amount, which would refund the
// budget. Callers compute amounts from file headers, so it fails closed
// rather than panics.
var errNegative = fmt.Errorf("%w: negative amount", ErrLimit)

// Hold reserves n bytes of memory until Release.
func (b *Budget) Hold(n int64) error {
	if n < 0 {
		return errNegative
	}
	if n > b.limits.Memory-b.held {
		return fmt.Errorf("%w: needs more than %d MB of memory", ErrLimit, b.limits.Memory>>20)
	}
	b.held += n
	return nil
}

// Release returns n bytes reserved with Hold. Releasing more than is held is
// a bug in the caller, and panics.
func (b *Budget) Release(n int64) {
	if n < 0 || n > b.held {
		panic(fmt.Sprintf("format: Release(%d) with %d bytes held", n, b.held))
	}
	b.held -= n
}

// Inflate counts n decompressed bytes.
func (b *Budget) Inflate(n int64) error {
	if n < 0 {
		return errNegative
	}
	if n > b.limits.Decompressed-b.decompressed {
		return fmt.Errorf("%w: decompresses to more than %d MB", ErrLimit, b.limits.Decompressed>>20)
	}
	b.decompressed += n
	return b.tick(n)
}

// Work counts n units of work.
func (b *Budget) Work(n int64) error {
	if n < 0 {
		return errNegative
	}
	if n > b.limits.Work-b.work {
		return fmt.Errorf("%w: needs more than %d units of work", ErrLimit, b.limits.Work)
	}
	b.work += n
	return b.tick(n)
}

// Enter starts processing a nested file; leave ends it. Calling leave again
// does nothing.
func (b *Budget) Enter() (leave func(), err error) {
	if b.depth >= b.limits.Depth {
		return nil, fmt.Errorf("%w: files nested more than %d deep", ErrLimit, b.limits.Depth)
	}
	b.depth++
	left := false
	return func() {
		if !left {
			left = true
			b.depth--
		}
	}, nil
}

// tick checks the context every checkEvery units.
func (b *Budget) tick(n int64) error {
	if b.sinceCheck += n; b.sinceCheck < checkEvery {
		return nil
	}
	b.sinceCheck = 0
	return b.ctx.Err()
}
