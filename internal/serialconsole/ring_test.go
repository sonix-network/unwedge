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
