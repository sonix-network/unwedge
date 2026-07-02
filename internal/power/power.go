// Package power controls the target's mains power through an APC switched rack
// PDU over SNMP. The vEdge 1000 under test is on a specific PDU outlet (outlet 3
// in the reference setup); power-cycling it is how we force the board back to
// U-Boot for netbooting a fresh kernel.
package power

import (
	"context"
	"fmt"
	"time"
)

// State is the power state of an outlet.
type State int

const (
	StateUnknown State = iota
	StateOn
	StateOff
)

func (s State) String() string {
	switch s {
	case StateOn:
		return "on"
	case StateOff:
		return "off"
	default:
		return "unknown"
	}
}

// Controller controls a single outlet.
type Controller interface {
	// Status queries the current outlet state.
	Status(ctx context.Context) (State, error)
	// On energizes the outlet.
	On(ctx context.Context) error
	// Off de-energizes the outlet.
	Off(ctx context.Context) error
	// Cycle powers the outlet off, waits offFor, then back on. If offFor<=0 a
	// controller-specific default is used.
	Cycle(ctx context.Context, offFor time.Duration) error
	// Close releases any underlying connection.
	Close() error
}

// DefaultOffDuration is how long Cycle keeps the outlet off by default. It is
// generous so the board's power rails fully discharge and it cold-boots.
const DefaultOffDuration = 5 * time.Second

// cycle is a shared helper implementing Cycle in terms of Off/On/Status.
func cycle(ctx context.Context, c Controller, offFor time.Duration, poll func(context.Context, State) error) error {
	if offFor <= 0 {
		offFor = DefaultOffDuration
	}
	if err := c.Off(ctx); err != nil {
		return fmt.Errorf("power off: %w", err)
	}
	if poll != nil {
		if err := poll(ctx, StateOff); err != nil {
			return err
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(offFor):
	}
	if err := c.On(ctx); err != nil {
		return fmt.Errorf("power on: %w", err)
	}
	if poll != nil {
		if err := poll(ctx, StateOn); err != nil {
			return err
		}
	}
	return nil
}
