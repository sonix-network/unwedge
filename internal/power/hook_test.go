package power

import (
	"context"
	"testing"
)

func TestHookFiresOnOffAndCycle(t *testing.T) {
	f := NewFake(StateOn)
	var losses int
	c := Hook(f, func() { losses++ })

	if err := c.Off(context.Background()); err != nil {
		t.Fatalf("off: %v", err)
	}
	if losses != 1 {
		t.Fatalf("after Off losses=%d, want 1", losses)
	}

	// Cycle must fire the hook exactly once, not twice (the concrete Cycle also
	// performs an Off internally, but that goes to the wrapped controller).
	if err := c.Cycle(context.Background(), 0); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if losses != 2 {
		t.Fatalf("after Cycle losses=%d, want 2", losses)
	}
}

func TestHookNotFiredOnOn(t *testing.T) {
	f := NewFake(StateOff)
	var losses int
	c := Hook(f, func() { losses++ })

	if err := c.On(context.Background()); err != nil {
		t.Fatalf("on: %v", err)
	}
	if losses != 0 {
		t.Fatalf("On should not signal power loss; losses=%d", losses)
	}
}

func TestHookNilReturnsUnwrapped(t *testing.T) {
	f := NewFake(StateOn)
	if got := Hook(f, nil); got != f {
		t.Fatalf("Hook with nil hook returned %v, want the original controller", got)
	}
}
