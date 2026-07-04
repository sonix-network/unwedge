// Package serialconsole owns a target's serial console. It continuously reads
// from an underlying transport (a real serial port, a PTY, or an in-memory pipe
// in tests), keeps a bounded scrollback ring buffer, fans live output out to any
// number of subscribers, and supports writing and regex pattern waiting. It is
// the foundation the U-Boot orchestration is built on.
package serialconsole

import (
	"context"
	"errors"
	"io"
	"regexp"
	"sync"
	"time"
)

// DefaultBufferBytes is the scrollback size used when none is configured.
const DefaultBufferBytes = 1 << 20 // 1 MiB

// subChanCap bounds how much un-consumed data a single subscriber may queue
// before it is considered too slow and detached. Each queued item is a chunk.
// It is a var (not a const) only so tests can lower it to exercise the
// slow-subscriber detach path deterministically.
var subChanCap = 1024

// ErrClosed is returned once the console transport has been closed.
var ErrClosed = errors.New("serialconsole: closed")

// subscriber receives copies of live console data.
type subscriber struct {
	ch     chan []byte
	closed bool
}

// Console manages a single serial transport.
type Console struct {
	mu       sync.Mutex
	rw       io.ReadWriteCloser
	ring     *ring
	subs     map[int]*subscriber
	nextID   int
	closed   bool
	closeErr error
	done     chan struct{}
}

// New wraps an already-open transport. bufferBytes<=0 uses DefaultBufferBytes.
// It starts a background reader goroutine immediately.
func New(rw io.ReadWriteCloser, bufferBytes int) *Console {
	if bufferBytes <= 0 {
		bufferBytes = DefaultBufferBytes
	}
	c := &Console{
		rw:   rw,
		ring: newRing(bufferBytes),
		subs: make(map[int]*subscriber),
		done: make(chan struct{}),
	}
	go c.readLoop()
	return c
}

func (c *Console) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := c.rw.Read(buf)
		if n > 0 {
			c.publish(buf[:n])
		}
		if err != nil {
			c.closeWithErr(err)
			return
		}
	}
}

func (c *Console) publish(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ring.write(p)
	for id, s := range c.subs {
		if s.closed {
			continue
		}
		cp := make([]byte, len(p))
		copy(cp, p)
		select {
		case s.ch <- cp:
		default:
			// Subscriber is too slow; detach it so it cannot stall the reader.
			s.closed = true
			close(s.ch)
			delete(c.subs, id)
		}
	}
}

func (c *Console) closeWithErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if err == io.EOF {
		err = ErrClosed
	}
	c.closeErr = err
	for id, s := range c.subs {
		if !s.closed {
			s.closed = true
			close(s.ch)
		}
		delete(c.subs, id)
	}
	close(c.done)
}

// Close stops the console and releases the underlying transport.
func (c *Console) Close() error {
	c.mu.Lock()
	rw := c.rw
	already := c.closed
	c.mu.Unlock()
	err := rw.Close()
	if !already {
		// readLoop will observe the closed transport and finalize; but if it is
		// blocked elsewhere, finalize directly.
		c.closeWithErr(ErrClosed)
	}
	return err
}

// Done returns a channel closed when the console stops.
func (c *Console) Done() <-chan struct{} { return c.done }

// Err returns the terminal error, if any, after the console has stopped.
func (c *Console) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

// Write sends bytes to the target.
func (c *Console) Write(p []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, ErrClosed
	}
	rw := c.rw
	c.mu.Unlock()
	return rw.Write(p)
}

// Snapshot returns a copy of the current scrollback. maxBytes<=0 returns all.
func (c *Console) Snapshot(maxBytes int) (data []byte, truncated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ring.snapshot(maxBytes)
}

// Reset clears the scrollback ring so subsequent Snapshot calls and
// replay-on-Subscribe start empty. Live subscribers are NOT affected: their
// channels keep receiving new data with no interruption. This is used to drop
// pre-power-cycle output so a freshly booted target's log starts clean instead
// of trailing every previous boot.
func (c *Console) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ring.reset()
}

// BufferedBytes reports how many bytes are currently in scrollback.
func (c *Console) BufferedBytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ring.size
}

// TotalWritten reports the total number of bytes ever published to the console
// (monotonic, except it is cleared by Reset). WaitForPattern uses it to bound
// how much output it missed if its subscription is detached mid-wait.
func (c *Console) TotalWritten() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ring.totalIn
}

// Subscription is a live feed of console output.
type Subscription struct {
	c  *Console
	id int
	ch chan []byte
}

// Subscribe registers a live feed. If replayBytes>0, that many bytes of recent
// scrollback are delivered on the channel first, before any new live data, with
// no gap in between. Always Close the subscription when done.
func (c *Console) Subscribe(replayBytes int) *Subscription {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &subscriber{ch: make(chan []byte, subChanCap)}
	if replayBytes > 0 {
		if snap, _ := c.ring.snapshot(replayBytes); len(snap) > 0 {
			s.ch <- snap
		}
	}
	if c.closed {
		// Deliver replay (already queued) then signal end.
		s.closed = true
		close(s.ch)
		id := c.nextID
		c.nextID++
		return &Subscription{c: c, id: id, ch: s.ch}
	}
	id := c.nextID
	c.nextID++
	c.subs[id] = s
	return &Subscription{c: c, id: id, ch: s.ch}
}

// C returns the channel of console chunks. It is closed when the subscription
// ends (either Close was called or the console stopped).
func (s *Subscription) C() <-chan []byte { return s.ch }

// Close detaches the subscription.
func (s *Subscription) Close() {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if sub, ok := s.c.subs[s.id]; ok && !sub.closed {
		sub.closed = true
		close(sub.ch)
		delete(s.c.subs, s.id)
	}
}

// maxPatternWindow bounds how much data WaitForPattern keeps in memory while
// scanning. Older data beyond this is dropped, retaining a tail overlap so a
// pattern spanning a boundary can still match.
const maxPatternWindow = 1 << 20

// patternScanOverlap is how far back of already-scanned data each incremental
// scan re-examines, so a match straddling the boundary between two chunks is
// still found. It bounds the longest pattern match that can span a chunk
// boundary; boot markers are far shorter than this.
const patternScanOverlap = 64 << 10

// patternScanner accumulates console output and searches it for a regexp
// incrementally: each new chunk is scanned together with only a bounded overlap
// of previously-scanned data, so scanning stays cheap no matter how much output
// has flowed. That keeps the pattern-waiter from becoming a slow subscriber.
type patternScanner struct {
	re      *regexp.Regexp
	acc     []byte
	scanned int // bytes of acc already searched (excluding the re-examined overlap)
}

// feed appends a chunk and returns the match if one has now appeared.
func (s *patternScanner) feed(chunk []byte) (string, bool) {
	s.acc = append(s.acc, chunk...)
	return s.scan()
}

// scan searches the not-yet-searched tail (plus the overlap) for the pattern.
func (s *patternScanner) scan() (string, bool) {
	start := s.scanned - patternScanOverlap
	if start < 0 {
		start = 0
	}
	if loc := s.re.FindIndex(s.acc[start:]); loc != nil {
		return string(s.acc[start+loc[0] : start+loc[1]]), true
	}
	s.scanned = len(s.acc)
	if len(s.acc) > maxPatternWindow {
		// Bound memory: keep the tail (with overlap) so boundary matches still work.
		keep := maxPatternWindow / 2
		dropped := len(s.acc) - keep
		s.acc = append([]byte(nil), s.acc[dropped:]...)
		if s.scanned -= dropped; s.scanned < 0 {
			s.scanned = 0
		}
	}
	return "", false
}

// reset discards accumulated data, e.g. before replaying a fresh snapshot.
func (s *patternScanner) reset() {
	s.acc = s.acc[:0]
	s.scanned = 0
}

// WaitForPattern blocks until re matches console output or ctx is done. If
// includeScrollback>0, that many bytes of existing scrollback are considered
// first. On match it returns the matched text and the accumulated context.
//
// If the pattern-waiter is detached for being a slow subscriber (an extreme
// output burst overflowing its queue) while the console is still open, it
// transparently re-subscribes and replays the output it missed rather than
// reporting the console as closed: such a detach is transient, and only a
// genuine transport close (c.Err() != nil) is fatal. This is what lets a
// wait_for_pattern armed across a target reboot survive the burst of boot output
// without aborting with "serialconsole: closed". See issue #28.
func (c *Console) WaitForPattern(ctx context.Context, re *regexp.Regexp, includeScrollback int) (match string, contextOut []byte, err error) {
	// armTotal marks how many bytes had been published when we armed, so a
	// re-subscribe after a detach replays only post-arm output (never pre-arm
	// history the caller did not ask to include).
	armTotal := c.TotalWritten()
	sc := &patternScanner{re: re}

	replay := includeScrollback
	for {
		sub := c.Subscribe(replay)
		matched, m, retry := c.drainInto(ctx, sub, sc)
		sub.Close()
		if matched {
			return m, sc.acc, nil
		}
		if ctx.Err() != nil {
			return "", sc.acc, ctx.Err()
		}
		if !retry {
			// The subscription ended and the console is genuinely closed.
			return "", sc.acc, ErrClosed
		}
		// Detached as a slow subscriber while the console is still alive. Re-arm,
		// replaying the output published since we started (bounded by the ring), so
		// a pattern that flew by during the gap is not missed. Reset the
		// accumulator; the replayed snapshot supersedes it.
		missed := c.TotalWritten() - armTotal
		if missed > maxPatternWindow {
			missed = maxPatternWindow
		}
		replay = int(missed)
		sc.reset()
	}
}

// drainInto feeds one subscription's output into sc. It returns matched=true
// with the match on success. Otherwise retry reports how the subscription ended:
// true if the subscriber was detached for being slow while the console is still
// open (the caller should re-subscribe), false if the console itself closed or
// ctx fired.
func (c *Console) drainInto(ctx context.Context, sub *Subscription, sc *patternScanner) (matched bool, match string, retry bool) {
	for {
		select {
		case <-ctx.Done():
			return false, "", false
		case chunk, ok := <-sub.C():
			if !ok {
				// A detach for slowness closes only this subscriber; the console
				// stays open (c.Err() == nil). A real transport close sets Err.
				return false, "", c.Err() == nil
			}
			if m, found := sc.feed(chunk); found {
				return true, m, false
			}
		}
	}
}

// WaitForPatternTimeout is a convenience wrapper using a timeout.
func (c *Console) WaitForPatternTimeout(re *regexp.Regexp, includeScrollback int, timeout time.Duration) (string, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.WaitForPattern(ctx, re, includeScrollback)
}
