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

	// Fetch both as a plain name and with a leading slash, as U-Boot sends
	// ("$serverip:/image.bin"). Both must serve the same bytes.
	for _, reqName := range []string{"vmlinux.bin", "/vmlinux.bin"} {
		wt, err := c.Receive(reqName, "octet")
		if err != nil {
			t.Fatalf("Receive(%q): %v", reqName, err)
		}
		var got bytes.Buffer
		if _, err := wt.WriteTo(&got); err != nil {
			t.Fatalf("WriteTo(%q): %v", reqName, err)
		}
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("fetch %q: got %d bytes, want %d", reqName, got.Len(), len(want))
		}
	}
}

func TestServerRejectsMissingAndTraversal(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	srv := NewServer(st, "", nil)

	// Missing file should error.
	if err := srv.readHandler("nope.bin", nopReaderFrom{}); err == nil {
		t.Fatal("expected error for missing file")
	}
	// Traversal is neutralized to a basename, which then does not exist -> error.
	// (The important property: it cannot escape the store directory.)
	if err := srv.readHandler("../../etc/passwd", nopReaderFrom{}); err == nil {
		t.Fatal("expected error for traversal")
	}
}

type nopReaderFrom struct{}

func (nopReaderFrom) ReadFrom(io.Reader) (int64, error) { return 0, nil }
