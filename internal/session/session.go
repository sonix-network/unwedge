// Package session implements the single-user hardware lock. One session holds
// the unit at a time; StartSession blocks until the lock is free. Every
// operational RPC refreshes the holder's TTL (via the server interceptor), and
// an idle session expires after the TTL so a crashed client cannot hold the
// lock forever.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// DefaultTTL is the idle timeout applied when none is configured.
const DefaultTTL = 5 * time.Minute

// Info is a snapshot of the current lock state.
type Info struct {
	Active    bool
	ID        string
	Owner     string
	StartedAt time.Time
	ExpiresAt time.Time
}

// BusyError is returned by Acquire when the lock could not be obtained in time.
type BusyError struct {
	Owner     string
	ExpiresAt time.Time
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("hardware is locked by %q (lock expires %s)", e.Owner, e.ExpiresAt.Format(time.RFC3339))
}

// Manager tracks the current session and serializes acquisition.
type Manager struct {
	ttl time.Duration
	now func() time.Time // injectable for tests
	gen func() string    // id generator, injectable for tests

	mu           sync.Mutex
	id           string
	owner        string
	startedAt    time.Time
	lastActivity time.Time
	activeCalls  int
	released     chan struct{} // closed when the current session ends; nil when free

	stop chan struct{}
	once sync.Once
}

// NewManager creates a manager with the given idle TTL and starts its reaper.
func NewManager(ttl time.Duration) *Manager {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	m := &Manager{
		ttl:  ttl,
		now:  time.Now,
		gen:  randomID,
		stop: make(chan struct{}),
	}
	go m.reaper()
	return m
}

// TTL returns the configured idle timeout.
func (m *Manager) TTL() time.Duration { return m.ttl }

// Close stops the background reaper.
func (m *Manager) Close() { m.once.Do(func() { close(m.stop) }) }

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// expiresAtLocked returns the current expiry; caller holds mu.
func (m *Manager) expiresAtLocked() time.Time { return m.lastActivity.Add(m.ttl) }

// expireIfIdleLocked releases the session if it has been idle past the TTL with
// no in-flight calls. Caller holds mu.
func (m *Manager) expireIfIdleLocked() {
	if m.id == "" || m.activeCalls > 0 {
		return
	}
	if m.now().After(m.expiresAtLocked()) {
		m.clearLocked()
	}
}

// clearLocked frees the lock and wakes waiters. Caller holds mu.
func (m *Manager) clearLocked() {
	m.id = ""
	m.owner = ""
	// activeCalls is a Manager-global counter tied to the holder id. If the lock
	// is cleared while a call is in flight (e.g. FinishSession from a killed
	// client races a still-tearing-down RPC), the eventual CallEnd carries the
	// old id and is skipped, so its decrement would otherwise leak into the next
	// session and pin activeCalls > 0 forever, wedging the reaper. Reset it here
	// so a new session always starts with a clean count. See issue #5.
	m.activeCalls = 0
	if m.released != nil {
		close(m.released)
		m.released = nil
	}
}

func (m *Manager) reaper() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.mu.Lock()
			m.expireIfIdleLocked()
			m.mu.Unlock()
		}
	}
}

// Acquire blocks until the lock is obtained, ctx is done, or (if wait > 0) the
// wait budget elapses. wait < 0 fails immediately if the lock is held.
func (m *Manager) Acquire(ctx context.Context, owner string, wait time.Duration) (Info, error) {
	var timeoutCh <-chan time.Time
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		timeoutCh = t.C
	}
	for {
		m.mu.Lock()
		m.expireIfIdleLocked()
		if m.id == "" {
			m.id = m.gen()
			m.owner = owner
			m.startedAt = m.now()
			m.lastActivity = m.now()
			m.activeCalls = 0 // invariant: a fresh session starts with no in-flight calls
			m.released = make(chan struct{})
			info := m.infoLocked()
			m.mu.Unlock()
			return info, nil
		}
		rel := m.released
		busy := &BusyError{Owner: m.owner, ExpiresAt: m.expiresAtLocked()}
		m.mu.Unlock()

		if wait < 0 {
			return Info{}, busy
		}
		select {
		case <-rel:
			// Lock freed; loop and try to grab it.
		case <-ctx.Done():
			return Info{}, ctx.Err()
		case <-timeoutCh:
			return Info{}, busy
		}
	}
}

// Finish releases the lock held by id. Releasing a free/expired lock is a no-op;
// releasing a lock held by a different id is an error.
func (m *Manager) Finish(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.id == "" || m.id == id {
		if m.id == id {
			m.clearLocked()
		}
		return nil
	}
	return fmt.Errorf("session: %q is not the lock holder", id)
}

// Refresh bumps the TTL for id. Returns an error if id is not the active,
// unexpired holder.
func (m *Manager) Refresh(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireIfIdleLocked()
	if m.id == "" || m.id != id {
		return fmt.Errorf("session: no active session %q", id)
	}
	m.lastActivity = m.now()
	return nil
}

// CallStart marks the beginning of an operational call: it validates id, bumps
// the TTL, and increments the in-flight counter (which blocks expiry until
// CallEnd). It returns an error if id is not the current holder.
func (m *Manager) CallStart(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireIfIdleLocked()
	if m.id == "" || m.id != id {
		return fmt.Errorf("session: no active session %q", id)
	}
	m.lastActivity = m.now()
	m.activeCalls++
	return nil
}

// CallEnd matches a prior CallStart.
func (m *Manager) CallEnd(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.id == id && m.activeCalls > 0 {
		m.activeCalls--
		m.lastActivity = m.now()
	}
}

// Info returns a snapshot of the current lock state.
func (m *Manager) Info() Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireIfIdleLocked()
	return m.infoLocked()
}

func (m *Manager) infoLocked() Info {
	if m.id == "" {
		return Info{}
	}
	return Info{
		Active:    true,
		ID:        m.id,
		Owner:     m.owner,
		StartedAt: m.startedAt,
		ExpiresAt: m.expiresAtLocked(),
	}
}
