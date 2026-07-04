package power

import (
	"context"
	"time"
)

// hooked wraps a Controller to run onPowerLoss just before the outlet is
// de-energized by Off or Cycle. The daemon uses it to clear console scrollback
// on a power cycle, so a freshly booted target's log starts at the last power
// event instead of accumulating every boot ever seen.
//
// The hook fires on Off and Cycle but not On: it marks the moment power is
// removed. Cycle is overridden (rather than relying on the Off it performs
// internally) because a concrete controller's Cycle calls its own Off, not this
// wrapper's, so the hook would otherwise never fire during a cycle.
type hooked struct {
	Controller
	onPowerLoss func()
}

// Hook wraps c so onPowerLoss runs immediately before power is removed by Off or
// Cycle. onPowerLoss must not block for long; it runs synchronously in the
// power path. If onPowerLoss is nil, c is returned unwrapped.
func Hook(c Controller, onPowerLoss func()) Controller {
	if onPowerLoss == nil {
		return c
	}
	return &hooked{Controller: c, onPowerLoss: onPowerLoss}
}

func (h *hooked) Off(ctx context.Context) error {
	h.onPowerLoss()
	return h.Controller.Off(ctx)
}

func (h *hooked) Cycle(ctx context.Context, offFor time.Duration) error {
	h.onPowerLoss()
	return h.Controller.Cycle(ctx, offFor)
}
