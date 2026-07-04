package serialconsole

import (
	"bytes"
	"io"
	"regexp"
	"sync"
	"testing"
	"time"
)

// fakeConn is an in-memory ReadWriteCloser used to drive the console in tests.
// Bytes pushed via feed() are returned by Read; bytes written are captured.
type fakeConn struct {
	mu      sync.Mutex
	readBuf []byte
	dataC   chan struct{}
	written bytes.Buffer
	closed  bool
	closedC chan struct{}
}

func newFakeConn() *fakeConn {
	return &fakeConn{dataC: make(chan struct{}, 1), closedC: make(chan struct{})}
}

func (f *fakeConn) feed(p []byte) {
	f.mu.Lock()
	f.readBuf = append(f.readBuf, p...)
	f.mu.Unlock()
	select {
	case f.dataC <- struct{}{}:
	default:
	}
}

func (f *fakeConn) Read(p []byte) (int, error) {
	for {
		f.mu.Lock()
		if len(f.readBuf) > 0 {
			n := copy(p, f.readBuf)
			f.readBuf = f.readBuf[n:]
			f.mu.Unlock()
			return n, nil
		}
		if f.closed {
			f.mu.Unlock()
			return 0, io.EOF
		}
		f.mu.Unlock()
		select {
		case <-f.dataC:
		case <-f.closedC:
		}
	}
}

func (f *fakeConn) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	return f.written.Write(p)
}

func (f *fakeConn) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	f.mu.Unlock()
	close(f.closedC)
	return nil
}

func (f *fakeConn) writtenBytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.written.Bytes()...)
}

func TestConsoleSnapshotAndWrite(t *testing.T) {
	fc := newFakeConn()
	c := New(fc, 4096)
	defer c.Close()

	fc.feed([]byte("hello "))
	fc.feed([]byte("world"))
	waitFor(t, func() bool {
		s, _ := c.Snapshot(0)
		return string(s) == "hello world"
	})

	if _, err := c.Write([]byte("printenv\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, func() bool { return string(fc.writtenBytes()) == "printenv\n" })
}

func TestWaitForPatternLive(t *testing.T) {
	fc := newFakeConn()
	c := New(fc, 4096)
	defer c.Close()

	re := regexp.MustCompile(`Hit ctrl-x to stop booting`)
	type res struct {
		match string
		err   error
	}
	done := make(chan res, 1)
	go func() {
		m, _, err := c.WaitForPatternTimeout(re, 0, 2*time.Second)
		done <- res{m, err}
	}()

	// Simulate boot output arriving in pieces, split across the pattern.
	fc.feed([]byte("U-Boot 2013.07\n"))
	time.Sleep(10 * time.Millisecond)
	fc.feed([]byte("Hit ctrl-x to "))
	time.Sleep(10 * time.Millisecond)
	fc.feed([]byte("stop booting 0\n"))

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("unexpected err: %v", r.err)
		}
		if r.match != "Hit ctrl-x to stop booting" {
			t.Fatalf("got match %q", r.match)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for pattern")
	}
}

func TestWaitForPatternScrollback(t *testing.T) {
	fc := newFakeConn()
	c := New(fc, 4096)
	defer c.Close()

	fc.feed([]byte("already saw the => prompt earlier\n"))
	waitFor(t, func() bool {
		s, _ := c.Snapshot(0)
		return bytes.Contains(s, []byte("=>"))
	})

	re := regexp.MustCompile(`=> ?`)
	m, _, err := c.WaitForPatternTimeout(re, 4096, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("expected scrollback match, got err %v", err)
	}
	if m == "" {
		t.Fatal("expected non-empty match from scrollback")
	}
}

func TestWaitForPatternTimeout(t *testing.T) {
	fc := newFakeConn()
	c := New(fc, 4096)
	defer c.Close()

	re := regexp.MustCompile(`never appears`)
	_, _, err := c.WaitForPatternTimeout(re, 0, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestSubscribeReplayThenLive(t *testing.T) {
	fc := newFakeConn()
	c := New(fc, 4096)
	defer c.Close()

	fc.feed([]byte("OLD"))
	waitFor(t, func() bool {
		s, _ := c.Snapshot(0)
		return string(s) == "OLD"
	})

	sub := c.Subscribe(4096)
	defer sub.Close()

	fc.feed([]byte("NEW"))

	var got []byte
	deadline := time.After(2 * time.Second)
	for string(got) != "OLDNEW" {
		select {
		case chunk := <-sub.C():
			got = append(got, chunk...)
		case <-deadline:
			t.Fatalf("got %q, want OLDNEW", got)
		}
	}
}

func TestConsoleCloseClosesSubscribers(t *testing.T) {
	fc := newFakeConn()
	c := New(fc, 4096)
	sub := c.Subscribe(0)
	c.Close()
	select {
	case _, ok := <-sub.C():
		if ok {
			// may receive buffered data first; drain until closed
			for range sub.C() {
			}
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber channel not closed after console close")
	}
}

func TestConsoleResetClearsScrollback(t *testing.T) {
	fc := newFakeConn()
	c := New(fc, 4096)
	defer c.Close()

	fc.feed([]byte("previous boot output"))
	waitFor(t, func() bool {
		s, _ := c.Snapshot(0)
		return string(s) == "previous boot output"
	})

	c.Reset()
	if s, trunc := c.Snapshot(0); len(s) != 0 || trunc {
		t.Fatalf("after reset snapshot=%q trunc=%v, want empty", s, trunc)
	}
	if n := c.BufferedBytes(); n != 0 {
		t.Fatalf("after reset BufferedBytes=%d, want 0", n)
	}

	// New output after the reset is captured as a clean log.
	fc.feed([]byte("fresh boot"))
	waitFor(t, func() bool {
		s, _ := c.Snapshot(0)
		return string(s) == "fresh boot"
	})
}

func TestConsoleResetKeepsLiveSubscriber(t *testing.T) {
	fc := newFakeConn()
	c := New(fc, 4096)
	defer c.Close()

	sub := c.Subscribe(0)
	defer sub.Close()

	// A reset must not interrupt an already-attached live feed: it only clears
	// scrollback, so a StreamConsole started before a power cycle keeps flowing.
	c.Reset()
	fc.feed([]byte("post-reset"))

	var got []byte
	deadline := time.After(2 * time.Second)
	for string(got) != "post-reset" {
		select {
		case chunk, ok := <-sub.C():
			if !ok {
				t.Fatalf("subscriber closed by reset; got %q", got)
			}
			got = append(got, chunk...)
		case <-deadline:
			t.Fatalf("got %q, want post-reset", got)
		}
	}
}

// TestSlowSubscriberDetachKeepsConsoleOpen pins the invariant WaitForPattern's
// recovery relies on: a subscriber detached for being too slow has only its own
// channel closed; the console itself stays open (Err() == nil). This is what
// lets WaitForPattern tell a transient detach apart from a real console close.
func TestSlowSubscriberDetachKeepsConsoleOpen(t *testing.T) {
	defer setSubChanCap(4)()

	fc := newFakeConn()
	c := New(fc, 1<<20)
	defer c.Close()

	sub := c.Subscribe(0) // deliberately never drained

	// Feed more full-size chunks than the queue can hold. Each is 4096 bytes so
	// the reader publishes them as separate chunks (no coalescing) and the
	// undrained subscriber's bounded queue overflows.
	chunk := bytes.Repeat([]byte("x"), 4096)
	for i := 0; i < subChanCap*3; i++ {
		fc.feed(chunk)
	}

	// The subscription channel is eventually closed by the detach.
	waitFor(t, func() bool {
		for {
			select {
			case _, ok := <-sub.C():
				if !ok {
					return true
				}
				// drained a buffered chunk; keep looking for the close
			default:
				return false
			}
		}
	})
	if err := c.Err(); err != nil {
		t.Fatalf("console must stay open after a slow-subscriber detach, got Err()=%v", err)
	}
}

// TestWaitForPatternSurvivesOutputBurst is the regression for issue #28: a heavy
// burst of console output must not make WaitForPattern abort with
// "serialconsole: closed" while the console is alive. The waiter must keep
// scanning (recovering via re-subscribe if its queue overflows) and match the
// marker that appears once output settles.
func TestWaitForPatternSurvivesOutputBurst(t *testing.T) {
	// A tiny queue makes a detach likely so the recovery path is exercised; a
	// large ring holds the whole burst so a replay-on-recovery can find the marker.
	defer setSubChanCap(2)()

	fc := newFakeConn()
	c := New(fc, 8<<20)
	defer c.Close()

	re := regexp.MustCompile(`Please press Enter to activate`)
	type res struct {
		m string
		e error
	}
	done := make(chan res, 1)
	go func() {
		m, _, e := c.WaitForPatternTimeout(re, 0, 5*time.Second)
		done <- res{m, e}
	}()

	// Sustained boot chatter that does not contain the marker.
	noise := bytes.Repeat([]byte("boot: initializing subsystem ....................\n"), 100)
	for i := 0; i < 400; i++ {
		fc.feed(noise)
	}
	// The marker finally appears when the console quiets down.
	fc.feed([]byte("procd: - init -\nPlease press Enter to activate this console.\n"))

	select {
	case r := <-done:
		if r.e != nil {
			t.Fatalf("wait aborted during output burst: %v", r.e)
		}
		if r.m != "Please press Enter to activate" {
			t.Fatalf("got match %q", r.m)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("timed out; wait did not survive the output burst")
	}
	if err := c.Err(); err != nil {
		t.Fatalf("console closed unexpectedly: %v", err)
	}
}

// TestPatternScannerIncremental checks the incremental scanner matches patterns
// split across feeds and spanning a chunk boundary without rescanning the whole
// window each time.
func TestPatternScannerIncremental(t *testing.T) {
	sc := &patternScanner{re: regexp.MustCompile(`Hit ctrl-x to stop booting`)}
	if _, ok := sc.feed([]byte("U-Boot 2013.07\n")); ok {
		t.Fatal("unexpected early match")
	}
	if _, ok := sc.feed([]byte("Hit ctrl-x to ")); ok {
		t.Fatal("unexpected match on partial pattern")
	}
	m, ok := sc.feed([]byte("stop booting 0\n"))
	if !ok {
		t.Fatal("pattern spanning two feeds not matched")
	}
	if m != "Hit ctrl-x to stop booting" {
		t.Fatalf("got match %q", m)
	}
}

func setSubChanCap(n int) func() {
	old := subChanCap
	subChanCap = n
	return func() { subChanCap = old }
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
