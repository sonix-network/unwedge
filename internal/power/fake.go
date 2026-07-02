package power

import (
	"context"
	"sync"
	"time"
)

// Fake is an in-memory Controller for tests and dry-run/offline operation.
type Fake struct {
	mu     sync.Mutex
	state  State
	Events []string // log of actions, in order
}

// NewFake returns a Fake starting in the given state.
func NewFake(initial State) *Fake { return &Fake{state: initial} }

func (f *Fake) Status(ctx context.Context) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

func (f *Fake) On(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = StateOn
	f.Events = append(f.Events, "on")
	return nil
}

func (f *Fake) Off(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = StateOff
	f.Events = append(f.Events, "off")
	return nil
}

func (f *Fake) Cycle(ctx context.Context, offFor time.Duration) error {
	// Use a short off time in the shared helper to keep tests fast.
	if offFor <= 0 {
		offFor = time.Millisecond
	}
	f.mu.Lock()
	f.Events = append(f.Events, "cycle")
	f.mu.Unlock()
	return cycle(ctx, f, offFor, nil)
}

func (f *Fake) Close() error { return nil }
