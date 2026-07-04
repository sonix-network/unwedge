package serialconsole

import (
	"bytes"
	"testing"
)

func TestRingBasic(t *testing.T) {
	r := newRing(8)
	r.write([]byte("abc"))
	got, trunc := r.snapshot(0)
	if string(got) != "abc" || trunc {
		t.Fatalf("got %q trunc=%v", got, trunc)
	}
}

func TestRingWrapAndTruncate(t *testing.T) {
	r := newRing(8)
	r.write([]byte("abcdef"))
	r.write([]byte("ghij")) // total 10 into cap 8 -> keep last 8 "cdefghij"
	got, trunc := r.snapshot(0)
	if string(got) != "cdefghij" {
		t.Fatalf("got %q", got)
	}
	if !trunc {
		t.Fatal("expected truncated=true")
	}
}

func TestRingWriteLargerThanCap(t *testing.T) {
	r := newRing(4)
	r.write([]byte("abcdefgh"))
	got, trunc := r.snapshot(0)
	if string(got) != "efgh" || !trunc {
		t.Fatalf("got %q trunc=%v", got, trunc)
	}
}

func TestRingSnapshotMaxBytes(t *testing.T) {
	r := newRing(16)
	r.write([]byte("0123456789"))
	got, trunc := r.snapshot(4)
	if !bytes.Equal(got, []byte("6789")) {
		t.Fatalf("got %q", got)
	}
	if !trunc {
		t.Fatal("expected truncated because maxBytes < size")
	}
}

func TestRingTotalIn(t *testing.T) {
	r := newRing(4)
	r.write([]byte("abcdef"))
	if r.totalIn != 6 {
		t.Fatalf("totalIn=%d", r.totalIn)
	}
}

func TestRingReset(t *testing.T) {
	r := newRing(4)
	r.write([]byte("abcdef")) // overflows cap -> truncated
	r.reset()
	got, trunc := r.snapshot(0)
	if len(got) != 0 || trunc {
		t.Fatalf("after reset got %q trunc=%v, want empty untruncated", got, trunc)
	}
	if r.totalIn != 0 {
		t.Fatalf("after reset totalIn=%d, want 0", r.totalIn)
	}
	// The ring is still usable and untruncated after a reset.
	r.write([]byte("xy"))
	got, trunc = r.snapshot(0)
	if string(got) != "xy" || trunc {
		t.Fatalf("post-reset write got %q trunc=%v", got, trunc)
	}
}
