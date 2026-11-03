package format

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

var errTest = errors.New("differs")

// copyIn rebuilds by copying the input in steps of n bytes, with extra
// appended at the end.
func copyIn(n int, extra string) func(in io.Reader, out io.Writer) error {
	return func(in io.Reader, out io.Writer) error {
		buf := make([]byte, n)
		for {
			k, err := in.Read(buf)
			if _, werr := out.Write(buf[:k]); werr != nil {
				return werr
			}
			if errors.Is(err, io.EOF) {
				_, err = io.WriteString(out, extra)
				return err
			}
			if err != nil {
				return err
			}
		}
	}
}

func TestVerifyFixedPoint(t *testing.T) {
	data := strings.Repeat("0123456789", 1000)
	tests := []struct {
		name    string
		input   string
		rebuild func(in io.Reader, out io.Writer) error
		want    error
	}{
		{"same, small steps", data, copyIn(7, ""), nil},
		{"same, large steps", data, copyIn(4096, ""), nil},
		{"output longer", data, copyIn(7, "x"), errTest},
		{"output shorter", data, func(in io.Reader, out io.Writer) error {
			_, err := io.Copy(out, io.LimitReader(in, 100))
			io.Copy(io.Discard, in)
			return err
		}, errTest},
		{"output first", data, func(in io.Reader, out io.Writer) error {
			if _, err := io.WriteString(out, data); err != nil {
				return err
			}
			_, err := io.Copy(io.Discard, in)
			return err
		}, nil},
		{"one byte differs", data, func(in io.Reader, out io.Writer) error {
			b, _ := io.ReadAll(in)
			b = bytes.Clone(b)
			b[5000] = 'x'
			_, err := out.Write(b)
			return err
		}, errTest},
		{"input runs too far ahead", strings.Repeat("a", maxLag+1), func(in io.Reader, _ io.Writer) error {
			_, err := io.Copy(io.Discard, in)
			return err
		}, errTest},
		{"rebuild error", data, func(io.Reader, io.Writer) error { return io.ErrClosedPipe }, io.ErrClosedPipe},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyFixedPoint(NewBudget(context.Background(), DefaultLimits), strings.NewReader(tt.input), errTest, tt.rebuild)
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// After the first difference, further reads fail, so a rebuild stops early.
func TestVerifyFixedPointStopsEarly(t *testing.T) {
	read := 0
	err := VerifyFixedPoint(NewBudget(context.Background(), DefaultLimits), strings.NewReader(strings.Repeat("a", 1<<20)), errTest, func(in io.Reader, out io.Writer) error {
		buf := make([]byte, 100)
		for {
			n, err := in.Read(buf)
			read += n
			if err != nil {
				return err
			}
			if _, err := out.Write(bytes.Repeat([]byte("b"), n)); err != nil {
				return err
			}
		}
	})
	if !errors.Is(err, errTest) {
		t.Errorf("err = %v, want %v", err, errTest)
	}
	if read > 200 {
		t.Errorf("read %d bytes after the first difference", read)
	}
}

// The bytes kept while one side runs ahead are held in the budget.
func TestVerifyFixedPointBudget(t *testing.T) {
	b := NewBudget(context.Background(), Limits{Memory: 64 << 10})
	err := VerifyFixedPoint(b, strings.NewReader(strings.Repeat("a", 1<<20)), errTest, func(in io.Reader, _ io.Writer) error {
		_, err := io.Copy(io.Discard, in)
		return err
	})
	if !errors.Is(err, ErrLimit) {
		t.Errorf("err = %v, want ErrLimit", err)
	}
	if b.held != 0 {
		t.Errorf("held %d bytes afterwards", b.held)
	}
}

// A budget error stays: a rebuild that swallows it can't drop the input bytes
// that went unrecorded and pass.
func TestVerifyFixedPointBudgetSticky(t *testing.T) {
	b := NewBudget(context.Background(), Limits{Memory: 4096})
	input := strings.Repeat("a", 4096) + strings.Repeat("s", 4096)
	err := VerifyFixedPoint(b, strings.NewReader(input), errTest, func(in io.Reader, out io.Writer) error {
		head := make([]byte, 4096)
		if _, err := io.ReadFull(in, head); err != nil {
			return err
		}
		_, _ = in.Read(make([]byte, 4096)) // fails on the budget, ignored
		_, err := out.Write(head)
		return err
	})
	if !errors.Is(err, ErrLimit) {
		t.Errorf("err = %v, want ErrLimit", err)
	}
}
