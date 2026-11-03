package format

// Buffer is a growing byte buffer whose capacity is held in a Budget. While
// it grows, the old and the new array are both held, since both are live
// during the copy. Release returns what it holds.
type Buffer struct {
	budget *Budget
	buf    []byte // buf[start:] holds the data
	start  int
}

// NewBuffer returns an empty buffer that holds its capacity in b.
func NewBuffer(b *Budget) *Buffer {
	return &Buffer{budget: b}
}

// Len returns the number of bytes in the buffer.
func (bf *Buffer) Len() int { return len(bf.buf) - bf.start }

// Bytes returns the data. It is valid until the next Write.
func (bf *Buffer) Bytes() []byte { return bf.buf[bf.start:] }

// Write appends p, holding more memory if the buffer has to grow.
func (bf *Buffer) Write(p []byte) (int, error) {
	n := bf.Len()
	need := n + len(p)
	switch {
	case len(bf.buf)+len(p) <= cap(bf.buf):
	case need <= cap(bf.buf)/2:
		// Room enough once the data moves to the front. Moving only when it
		// fills at most half keeps the copying linear in what is written.
		copy(bf.buf, bf.buf[bf.start:])
		bf.buf, bf.start = bf.buf[:n], 0
	default:
		newCap := max(2*cap(bf.buf), need, 512)
		if err := bf.budget.Hold(int64(newCap)); err != nil {
			return 0, err
		}
		nb := make([]byte, n, newCap)
		copy(nb, bf.Bytes())
		bf.budget.Release(int64(cap(bf.buf)))
		bf.buf, bf.start = nb, 0
	}
	bf.buf = append(bf.buf, p...)
	return len(p), nil
}

// Discard drops the first n bytes, which must be at most Len.
func (bf *Buffer) Discard(n int) {
	bf.start += n
	if bf.start == len(bf.buf) {
		bf.buf, bf.start = bf.buf[:0], 0
	}
}

// Release frees the buffer and returns its memory to the budget. The buffer
// can be used again afterwards.
func (bf *Buffer) Release() {
	bf.budget.Release(int64(cap(bf.buf)))
	bf.buf, bf.start = nil, 0
}
