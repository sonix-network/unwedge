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
const subChanCap = 1024

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

// BufferedBytes reports how many bytes are currently in scrollback.
func (c *Console) BufferedBytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ring.size
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

// WaitForPattern blocks until re matches console output or ctx is done. If
// includeScrollback>0, that many bytes of existing scrollback are considered
// first. On match it returns the matched text and the accumulated context.
func (c *Console) WaitForPattern(ctx context.Context, re *regexp.Regexp, includeScrollback int) (match string, contextOut []byte, err error) {
	sub := c.Subscribe(includeScrollback)
	defer sub.Close()

	var acc []byte
	scan := func() (string, bool) {
		if loc := re.FindIndex(acc); loc != nil {
			return string(acc[loc[0]:loc[1]]), true
		}
		return "", false
	}

	for {
		select {
		case <-ctx.Done():
			return "", acc, ctx.Err()
		case chunk, ok := <-sub.C():
			if !ok {
				// Console closed; do a final scan then report closure.
				if m, found := scan(); found {
					return m, acc, nil
				}
				return "", acc, ErrClosed
			}
			acc = append(acc, chunk...)
			if m, found := scan(); found {
				return m, acc, nil
			}
			if len(acc) > maxPatternWindow {
				// Keep the tail so boundary-spanning matches still work.
				keep := maxPatternWindow / 2
				acc = append([]byte(nil), acc[len(acc)-keep:]...)
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
