package format

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"testing"
)

// Buffer keeps the same bytes as a plain slice under random writes and
// discards, and holds no more than its capacity.
func TestBuffer(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	b := NewBudget(context.Background(), Limits{Memory: 1 << 20})
	bf := NewBuffer(b)
	var want []byte
	for range 10_000 {
		if rng.IntN(3) == 0 && len(want) > 0 {
			n := rng.IntN(len(want) + 1)
			bf.Discard(n)
			want = want[n:]
			continue
		}
		p := make([]byte, rng.IntN(300))
		for i := range p {
			p[i] = byte(rng.Uint32())
		}
		if _, err := bf.Write(p); err != nil {
			t.Fatal(err)
		}
		want = append(want, p...)
		if !bytes.Equal(bf.Bytes(), want) {
			t.Fatal("buffer holds other bytes")
		}
		if b.held != int64(cap(bf.buf)) {
			t.Fatalf("held %d bytes for a capacity of %d", b.held, cap(bf.buf))
		}
	}
	bf.Release()
	if b.held != 0 {
		t.Errorf("held %d bytes after Release", b.held)
	}
}

// Growing holds the old and the new array, so a buffer can't grow past half
// the limit.
func TestBufferLimit(t *testing.T) {
	bf := NewBuffer(NewBudget(context.Background(), Limits{Memory: 3000}))
	var err error
	for range 100 {
		if _, err = bf.Write(make([]byte, 100)); err != nil {
			break
		}
	}
	if !errors.Is(err, ErrLimit) || bf.Len() > 1500 {
		t.Errorf("err = %v with %d bytes, want ErrLimit before 1500", err, bf.Len())
	}
}
