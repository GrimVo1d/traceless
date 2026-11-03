package format

import (
	"context"
	"errors"
	"testing"
)

func TestBudgetLimits(t *testing.T) {
	b := NewBudget(context.Background(), Limits{Memory: 100, Decompressed: 100, Work: 100, Depth: 1})

	if err := b.Hold(60); err != nil {
		t.Fatalf("Hold(60): %v", err)
	}
	if err := b.Hold(41); !errors.Is(err, ErrLimit) {
		t.Errorf("Hold over the limit = %v, want ErrLimit", err)
	}
	b.Release(60)
	if err := b.Hold(100); err != nil {
		t.Errorf("Hold after Release: %v", err)
	}

	if err := b.Inflate(100); err != nil {
		t.Errorf("Inflate(100): %v", err)
	}
	if err := b.Inflate(1); !errors.Is(err, ErrLimit) {
		t.Errorf("Inflate over the limit = %v, want ErrLimit", err)
	}
	if err := b.Work(100); err != nil {
		t.Errorf("Work(100): %v", err)
	}
	if err := b.Work(1); !errors.Is(err, ErrLimit) {
		t.Errorf("Work over the limit = %v, want ErrLimit", err)
	}

	leave, err := b.Enter()
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	if _, err := b.Enter(); !errors.Is(err, ErrLimit) {
		t.Errorf("Enter past the depth = %v, want ErrLimit", err)
	}
	leave()
	if _, err := b.Enter(); err != nil {
		t.Errorf("Enter after leave: %v", err)
	}
}

// Large requests can't overflow the counters into passing.
func TestBudgetOverflow(t *testing.T) {
	b := NewBudget(context.Background(), DefaultLimits)
	if err := b.Hold(1 << 62); !errors.Is(err, ErrLimit) {
		t.Errorf("Hold(2^62) = %v, want ErrLimit", err)
	}
	if err := b.Hold(1 << 62); !errors.Is(err, ErrLimit) {
		t.Errorf("second Hold(2^62) = %v, want ErrLimit", err)
	}
}

// Negative amounts can't refund the budget, and leaving twice doesn't free a
// nesting level.
func TestBudgetMisuse(t *testing.T) {
	b := NewBudget(nil, Limits{Memory: 100, Decompressed: 100, Work: 100, Depth: 1}) //nolint:staticcheck // nil means Background
	for name, fn := range map[string]func(int64) error{"Hold": b.Hold, "Inflate": b.Inflate, "Work": b.Work} {
		if err := fn(-1 << 40); !errors.Is(err, ErrLimit) {
			t.Errorf("%s(-2^40) = %v, want ErrLimit", name, err)
		}
	}
	leave, err := b.Enter()
	if err != nil {
		t.Fatal(err)
	}
	leave()
	leave()
	if _, err := b.Enter(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Enter(); !errors.Is(err, ErrLimit) {
		t.Errorf("Enter past the depth after a double leave = %v, want ErrLimit", err)
	}
	defer func() {
		if recover() == nil {
			t.Error("Release past what is held didn't panic")
		}
	}()
	b.Release(1)
}

// Work and Inflate notice a canceled context without reading any input.
func TestBudgetCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := NewBudget(ctx, DefaultLimits)
	cancel()
	var err error
	for range checkEvery {
		if err = b.Work(1); err != nil {
			break
		}
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Work after cancel = %v, want context.Canceled", err)
	}
}
