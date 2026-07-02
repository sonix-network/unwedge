package tftp

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path"
	"strings"

	"github.com/pin/tftp/v3"
)

// Server is a read-only TFTP server that serves images from a Store. The target
// board's U-Boot fetches kernels from it during netboot. Writes are refused.
type Server struct {
	store  *Store
	addr   string
	logger *slog.Logger
	srv    *tftp.Server
}

// NewServer builds a TFTP server. addr is a UDP listen address (e.g. ":69" or
// "0.0.0.0:6969"). Serving on port 69 requires privileges or CAP_NET_BIND.
func NewServer(store *Store, addr string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{store: store, addr: addr, logger: logger}
	s.srv = tftp.NewServer(s.readHandler, nil) // nil write handler => read-only
	return s
}

// readHandler streams a requested image to the client.
func (s *Server) readHandler(filename string, rf io.ReaderFrom) error {
	// TFTP clients (notably U-Boot, which requests "$serverip:/image.bin") send
	// a leading slash and sometimes a directory prefix. Serve by basename like a
	// standard tftpd; Store.Path still rejects "."/".." and other junk.
	name := path.Base(strings.TrimSpace(filename))
	fpath, err := s.store.Path(name)
	if err != nil {
		s.logger.Warn("tftp rejected request", "file", filename, "err", err)
		return err
	}
	f, err := os.Open(fpath)
	if err != nil {
		s.logger.Warn("tftp file open failed", "file", filename, "err", err)
		return fmt.Errorf("open %s: %w", filename, err)
	}
	defer f.Close()

	// Advertise the size so U-Boot can show progress / preallocate.
	if fi, err := f.Stat(); err == nil {
		if ot, ok := rf.(tftp.OutgoingTransfer); ok {
			ot.SetSize(fi.Size())
		}
	}
	n, err := rf.ReadFrom(f)
	if err != nil {
		s.logger.Warn("tftp transfer failed", "file", filename, "sent", n, "err", err)
		return err
	}
	s.logger.Info("tftp served image", "file", filename, "bytes", n)
	return nil
}

// ListenAndServe blocks serving requests until Shutdown is called.
func (s *Server) ListenAndServe() error {
	s.logger.Info("tftp server listening", "addr", s.addr, "dir", s.store.Dir())
	return s.srv.ListenAndServe(s.addr)
}

// Serve serves on an already-bound packet connection. Useful for binding an
// ephemeral port (":0") and learning the address, e.g. in tests.
func (s *Server) Serve(conn net.PacketConn) error {
	s.logger.Info("tftp server serving", "addr", conn.LocalAddr(), "dir", s.store.Dir())
	return s.srv.Serve(conn)
}

// Shutdown stops the server.
func (s *Server) Shutdown() {
	if s.srv != nil {
		s.srv.Shutdown()
	}
}
