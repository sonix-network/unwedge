package serialconsole

// ring is a fixed-capacity byte ring buffer that retains the most recently
// written bytes. It tracks the total number of bytes ever written so callers
// can tell whether older data has been evicted (truncated).
type ring struct {
	buf       []byte
	size      int   // number of valid bytes currently stored (<= cap)
	start     int   // index of the oldest byte
	totalIn   int64 // total bytes ever written
	truncated bool  // true once any byte has been evicted
}

func newRing(capacity int) *ring {
	if capacity < 1 {
		capacity = 1
	}
	return &ring{buf: make([]byte, capacity)}
}

func (r *ring) write(p []byte) {
	r.totalIn += int64(len(p))
	c := len(r.buf)
	// If the incoming data is larger than the buffer, keep only its tail.
	if len(p) >= c {
		copy(r.buf, p[len(p)-c:])
		r.start = 0
		r.size = c
		r.truncated = true
		return
	}
	// Write position is where the next byte goes.
	end := (r.start + r.size) % c
	n := copy(r.buf[end:], p)
	if n < len(p) {
		copy(r.buf, p[n:])
	}
	free := c - r.size
	if len(p) > free {
		// Overwrote some old bytes: advance start and clamp size.
		over := len(p) - free
		r.start = (r.start + over) % c
		r.size = c
		r.truncated = true
	} else {
		r.size += len(p)
	}
}

// reset discards all buffered bytes, returning the ring to its initial empty
// state. The truncation and total-bytes counters are cleared too, so after a
// reset a snapshot reads as a fresh, untruncated log.
func (r *ring) reset() {
	r.size = 0
	r.start = 0
	r.totalIn = 0
	r.truncated = false
}

// snapshot returns a copy of the buffered bytes in order. If maxBytes > 0 and
// smaller than the buffered size, only the most recent maxBytes are returned.
// The second return value reports whether any data (from the ring as a whole)
// has been evicted over the lifetime of the buffer.
func (r *ring) snapshot(maxBytes int) ([]byte, bool) {
	n := r.size
	if maxBytes > 0 && maxBytes < n {
		n = maxBytes
	}
	out := make([]byte, n)
	// Copy the last n bytes.
	begin := (r.start + (r.size - n)) % len(r.buf)
	first := copy(out, r.buf[begin:])
	if first < n {
		copy(out[first:], r.buf)
	}
	truncated := r.truncated || (maxBytes > 0 && maxBytes < r.size)
	return out, truncated
}
