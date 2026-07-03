package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAcquireAndFinish(t *testing.T) {
	m := NewManager(time.Minute)
	defer m.Close()
	info, err := m.Acquire(context.Background(), "a", -1)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !info.Active || info.ID == "" || info.Owner != "a" {
		t.Fatalf("info = %+v", info)
	}
	if got := m.Info(); got.ID != info.ID {
		t.Fatalf("Info mismatch")
	}
	if err := m.Finish(info.ID); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if m.Info().Active {
		t.Fatal("expected free after Finish")
	}
}

func TestAcquireNonBlockingBusy(t *testing.T) {
	m := NewManager(time.Minute)
	defer m.Close()
	a, _ := m.Acquire(context.Background(), "a", -1)
	_, err := m.Acquire(context.Background(), "b", -1)
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("want BusyError, got %v", err)
	}
	if busy.Owner != "a" {
		t.Fatalf("busy owner = %q", busy.Owner)
	}
	_ = a
}

func TestAcquireBlocksUntilFree(t *testing.T) {
	m := NewManager(time.Minute)
	defer m.Close()
	a, _ := m.Acquire(context.Background(), "a", 0)

	got := make(chan Info, 1)
	go func() {
		info, err := m.Acquire(context.Background(), "b", 0)
		if err != nil {
			t.Errorf("second Acquire: %v", err)
		}
		got <- info
	}()

	// Should still be blocked.
	select {
	case <-got:
		t.Fatal("second Acquire returned while lock held")
	case <-time.After(100 * time.Millisecond):
	}

	if err := m.Finish(a.ID); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	select {
	case info := <-got:
		if info.Owner != "b" || info.ID == a.ID {
			t.Fatalf("second session = %+v", info)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Acquire did not proceed after Finish")
	}
}

func TestAcquireWaitTimeout(t *testing.T) {
	m := NewManager(time.Minute)
	defer m.Close()
	m.Acquire(context.Background(), "a", 0)
	start := time.Now()
	_, err := m.Acquire(context.Background(), "b", 80*time.Millisecond)
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("want BusyError, got %v", err)
	}
	if time.Since(start) < 70*time.Millisecond {
		t.Fatalf("returned too early")
	}
}

func TestAcquireContextCancel(t *testing.T) {
	m := NewManager(time.Minute)
	defer m.Close()
	m.Acquire(context.Background(), "a", 0)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := m.Acquire(ctx, "b", 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestIdleExpiry(t *testing.T) {
	m := NewManager(60 * time.Millisecond)
	defer m.Close()
	a, _ := m.Acquire(context.Background(), "a", -1)
	// Wait past TTL; reaper (1s tick) may lag, but Info/Acquire expire lazily.
	time.Sleep(120 * time.Millisecond)
	if m.Info().Active {
		t.Fatal("expected lock to expire when idle")
	}
	// A new owner can now acquire.
	b, err := m.Acquire(context.Background(), "b", -1)
	if err != nil || b.ID == a.ID {
		t.Fatalf("re-acquire after expiry failed: %v id=%v", err, b.ID)
	}
}

func TestRefreshPreventsExpiry(t *testing.T) {
	m := NewManager(80 * time.Millisecond)
	defer m.Close()
	a, _ := m.Acquire(context.Background(), "a", -1)
	for i := 0; i < 4; i++ {
		time.Sleep(30 * time.Millisecond)
		if err := m.Refresh(a.ID); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
	}
	if !m.Info().Active {
		t.Fatal("lock should still be held after refreshes")
	}
}

func TestActiveCallBlocksExpiry(t *testing.T) {
	m := NewManager(50 * time.Millisecond)
	defer m.Close()
	a, _ := m.Acquire(context.Background(), "a", -1)
	if err := m.CallStart(a.ID); err != nil {
		t.Fatalf("CallStart: %v", err)
	}
	time.Sleep(120 * time.Millisecond) // well past TTL
	if !m.Info().Active {
		t.Fatal("in-flight call must keep the lock alive")
	}
	m.CallEnd(a.ID)
	time.Sleep(120 * time.Millisecond)
	if m.Info().Active {
		t.Fatal("lock should expire after the call ends and TTL passes")
	}
}

// TestFinishDuringCallDoesNotWedgeNextSession reproduces issue #5: a call in
// flight when the holder is cleared (e.g. a killed client's FinishSession racing
// its still-tearing-down RPC) must not leak the activeCalls decrement into the
// next session and pin it as permanently non-expiring.
func TestFinishDuringCallDoesNotWedgeNextSession(t *testing.T) {
	m := NewManager(50 * time.Millisecond)
	defer m.Close()

	// Session A holds the lock with a call in flight.
	a, _ := m.Acquire(context.Background(), "a", -1)
	if err := m.CallStart(a.ID); err != nil {
		t.Fatalf("CallStart(a): %v", err)
	}
	// A's client dies: FinishSession clears the lock while the call is still
	// in flight (activeCalls == 1).
	if err := m.Finish(a.ID); err != nil {
		t.Fatalf("Finish(a): %v", err)
	}

	// Session B acquires the now-free lock.
	b, err := m.Acquire(context.Background(), "b", -1)
	if err != nil {
		t.Fatalf("Acquire(b): %v", err)
	}
	// A's original call finally returns; its CallEnd carries the stale id and
	// must not affect B's counter.
	m.CallEnd(a.ID)

	// B is idle and past its TTL, so it must expire.
	time.Sleep(120 * time.Millisecond)
	if m.Info().Active {
		t.Fatal("idle session B never expired: activeCalls leaked across the session boundary (issue #5)")
	}
	// And the hardware is reclaimable.
	if _, err := m.Acquire(context.Background(), "c", -1); err != nil {
		t.Fatalf("re-acquire after expiry failed: %v", err)
	}
	_ = b
}

func TestRefreshRejectsWrongID(t *testing.T) {
	m := NewManager(time.Minute)
	defer m.Close()
	m.Acquire(context.Background(), "a", -1)
	if err := m.Refresh("bogus"); err == nil {
		t.Fatal("expected error refreshing non-holder id")
	}
	if err := m.CallStart("bogus"); err == nil {
		t.Fatal("expected error CallStart with non-holder id")
	}
}

func TestFinishWrongIDErrors(t *testing.T) {
	m := NewManager(time.Minute)
	defer m.Close()
	m.Acquire(context.Background(), "a", -1)
	if err := m.Finish("bogus"); err == nil {
		t.Fatal("expected error finishing someone else's lock")
	}
	// Finishing a free lock is a no-op.
	m2 := NewManager(time.Minute)
	defer m2.Close()
	if err := m2.Finish("anything"); err != nil {
		t.Fatalf("finishing free lock should be nil, got %v", err)
	}
}
