package tftp

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pin/tftp/v3"
)

// TestServerRoundTrip serves an image and fetches it back with a TFTP client,
// mirroring what the target's U-Boot does during netboot.
func TestServerRoundTrip(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	want := bytes.Repeat([]byte("KERNEL"), 5000) // ~30 KiB, exercises multiple blocks
	if _, err := st.Save("vmlinux.bin", bytes.NewReader(want), false); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Bind an ephemeral UDP port and serve on it.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	srv := NewServer(st, "", nil)
	go func() { _ = srv.Serve(pc) }()
	defer srv.Shutdown()
	addr := pc.LocalAddr().(*net.UDPAddr)

	// Fetch it back.
	c, err := tftp.NewClient(addr.String())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.SetTimeout(2 * time.Second)
	wt, err := c.Receive("vmlinux.bin", "octet")
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	var got bytes.Buffer
	if _, err := wt.WriteTo(&got); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("fetched %d bytes, want %d", got.Len(), len(want))
	}
}

func TestServerRejectsMissingAndTraversal(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	srv := NewServer(st, "", nil)

	// Missing file should error.
	if err := srv.readHandler("nope.bin", nopReaderFrom{}); err == nil {
		t.Fatal("expected error for missing file")
	}
	// Traversal should error before touching the filesystem.
	if err := srv.readHandler("../etc/passwd", nopReaderFrom{}); err == nil {
		t.Fatal("expected error for traversal")
	}
}

type nopReaderFrom struct{}

func (nopReaderFrom) ReadFrom(io.Reader) (int64, error) { return 0, nil }
