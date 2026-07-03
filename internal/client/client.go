// Package client provides a shared gRPC client used by the CLI and the MCP
// bridge to talk to a unwedged daemon, including TLS/mTLS setup and a few
// higher-level convenience helpers (image upload, boot-event streaming).
package client

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	unwedgev1 "github.com/sonix-network/unwedge/gen/unwedge/v1"
	"github.com/sonix-network/unwedge/internal/tlsutil"
)

// sessionMetadataKey must match server.SessionMetadataKey; the session ID is
// sent on every operational call so the daemon can enforce the hardware lock.
const sessionMetadataKey = "unwedge-session-id"

// Options configures a client connection.
type Options struct {
	// Address is the daemon's host:port.
	Address string
	// NoTLS dials without transport security (local/testing only).
	NoTLS bool
	// TLS client options (CA, optional client cert for mTLS, server name).
	CAFile     string
	CertFile   string
	KeyFile    string
	ServerName string
	// Insecure skips server certificate verification (development only).
	Insecure bool
	// Dialer, if set, overrides how the underlying connection is made (e.g. an
	// in-memory bufconn in tests). When set, NoTLS is implied.
	Dialer func(context.Context, string) (net.Conn, error)
}

// Client wraps the gRPC connection and generated stub.
type Client struct {
	conn *grpc.ClientConn
	API  unwedgev1.UnwedgeClient

	mu        sync.Mutex
	sessionID string // set while holding a session; injected into call metadata
}

func (c *Client) session() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *Client) setSession(id string) {
	c.mu.Lock()
	c.sessionID = id
	c.mu.Unlock()
}

// withSession adds the current session ID (if any) to the outgoing metadata.
func (c *Client) withSession(ctx context.Context) context.Context {
	if id := c.session(); id != "" {
		return metadata.AppendToOutgoingContext(ctx, sessionMetadataKey, id)
	}
	return ctx
}

func (c *Client) unaryInterceptor(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	return invoker(c.withSession(ctx), method, req, reply, cc, opts...)
}

func (c *Client) streamInterceptor(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return streamer(c.withSession(ctx), desc, cc, method, opts...)
}

// Dial connects to the daemon.
func Dial(o Options) (*Client, error) {
	if o.Address == "" {
		return nil, fmt.Errorf("client: address required")
	}
	var dialOpts []grpc.DialOption
	if o.Dialer != nil {
		dialOpts = append(dialOpts, grpc.WithContextDialer(o.Dialer))
	}
	var dialOpt grpc.DialOption
	if o.NoTLS || o.Dialer != nil {
		dialOpt = grpc.WithTransportCredentials(insecure.NewCredentials())
	} else {
		creds, err := tlsutil.ClientCredentials(tlsutil.ClientOptions{
			CAFile:             o.CAFile,
			CertFile:           o.CertFile,
			KeyFile:            o.KeyFile,
			ServerName:         o.ServerName,
			InsecureSkipVerify: o.Insecure,
		})
		if err != nil {
			return nil, err
		}
		dialOpt = grpc.WithTransportCredentials(creds)
	}
	dialOpts = append(dialOpts, dialOpt)
	c := &Client{}
	dialOpts = append(dialOpts,
		grpc.WithChainUnaryInterceptor(c.unaryInterceptor),
		grpc.WithChainStreamInterceptor(c.streamInterceptor),
	)
	conn, err := grpc.NewClient(o.Address, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("client: dial %s: %w", o.Address, err)
	}
	c.conn = conn
	c.API = unwedgev1.NewUnwedgeClient(conn)
	return c, nil
}

// Session is a snapshot of an acquired hardware lock.
type Session struct {
	ID        string
	ExpiresAt time.Time
	TTL       time.Duration
}

// Acquire obtains the hardware lock and stores the session ID so it is attached
// to all subsequent calls. owner is a label shown in GetStatus. wait bounds how
// long to block for a held lock: 0 blocks until ctx, negative fails fast.
func (c *Client) Acquire(ctx context.Context, owner string, wait time.Duration) (Session, error) {
	// Encode wait without letting sub-millisecond durations collapse a negative
	// (fail-fast) or small positive wait into 0 (which means "block until ctx").
	var waitMs int64
	switch {
	case wait < 0:
		waitMs = -1
	case wait == 0:
		waitMs = 0
	default:
		if waitMs = wait.Milliseconds(); waitMs == 0 {
			waitMs = 1
		}
	}
	resp, err := c.API.StartSession(ctx, &unwedgev1.StartSessionRequest{
		Owner: owner, WaitTimeoutMs: waitMs,
	})
	if err != nil {
		return Session{}, err
	}
	c.setSession(resp.GetSessionId())
	return Session{
		ID:        resp.GetSessionId(),
		ExpiresAt: time.UnixMilli(resp.GetExpiresAtUnixMs()),
		TTL:       time.Duration(resp.GetTtlMs()) * time.Millisecond,
	}, nil
}

// Release finishes the current session, if any. It clears the stored ID even if
// the RPC fails (the lock will expire on its own).
func (c *Client) Release(ctx context.Context) error {
	id := c.session()
	if id == "" {
		return nil
	}
	c.setSession("")
	_, err := c.API.FinishSession(ctx, &unwedgev1.FinishSessionRequest{SessionId: id})
	return err
}

// Ping refreshes the current session's TTL.
func (c *Client) Ping(ctx context.Context) error {
	id := c.session()
	if id == "" {
		return nil
	}
	_, err := c.API.Ping(ctx, &unwedgev1.PingRequest{SessionId: id})
	return err
}

// HasSession reports whether a session is currently held.
func (c *Client) HasSession() bool { return c.session() != "" }

// ClearSession forgets the stored session ID without contacting the server.
// Use it when the daemon reports the session was lost (e.g. it expired) so the
// next call re-acquires.
func (c *Client) ClearSession() { c.setSession("") }

// StartKeepalive pings the session every interval until the returned stop func
// is called. Use it for the duration of an active operation so long local work
// does not let the lock expire. Do NOT use it across idle periods where the
// lock is meant to auto-release.
func (c *Client) StartKeepalive(interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
				_ = c.Ping(pctx)
				pcancel()
			}
		}
	}()
	return cancel
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// UploadImageFile streams a local file to the daemon's image store.
func (c *Client) UploadImageFile(ctx context.Context, path string, overwrite bool) (*unwedgev1.UploadImageResponse, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("client: open %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	stream, err := c.API.UploadImage(ctx)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	if err := stream.Send(&unwedgev1.UploadImageRequest{
		Payload: &unwedgev1.UploadImageRequest_Metadata_{
			Metadata: &unwedgev1.UploadImageRequest_Metadata{
				Name: name, Size: fi.Size(), Overwrite: overwrite,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("client: send metadata: %w", err)
	}
	buf := make([]byte, 64*1024)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&unwedgev1.UploadImageRequest{
				Payload: &unwedgev1.UploadImageRequest_Chunk{Chunk: buf[:n]},
			}); err != nil {
				return nil, fmt.Errorf("client: send chunk: %w", err)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("client: read %s: %w", path, rerr)
		}
	}
	return stream.CloseAndRecv()
}

// BootEventHandler receives streamed boot events.
type BootEventHandler func(*unwedgev1.BootEvent)

// Netboot runs a netboot and forwards each event to h. It returns the terminal
// error, if any (the stream RPC status).
func (c *Client) Netboot(ctx context.Context, req *unwedgev1.NetbootRequest, h BootEventHandler) error {
	stream, err := c.API.Netboot(ctx, req)
	if err != nil {
		return err
	}
	return drainBootEvents(stream, h)
}

// InterruptBoot runs an interrupt-boot and forwards each event to h.
func (c *Client) InterruptBoot(ctx context.Context, req *unwedgev1.InterruptBootRequest, h BootEventHandler) error {
	stream, err := c.API.InterruptBoot(ctx, req)
	if err != nil {
		return err
	}
	return drainBootEvents(stream, h)
}

// Tunnel proxies a raw byte stream between in/out and the target's SSH port
// (or hostOverride "host[:port]") through the daemon, returning when either end
// closes. It backs an OpenSSH ProxyCommand so a local ssh/scp can reach the
// target through the daemon.
func (c *Client) Tunnel(ctx context.Context, hostOverride string, in io.Reader, out io.Writer) error {
	stream, err := c.API.Tunnel(ctx)
	if err != nil {
		return err
	}
	// The first message opens the tunnel and selects the dial target.
	if err := stream.Send(&unwedgev1.TunnelChunk{HostOverride: hostOverride}); err != nil {
		return err
	}
	errc := make(chan error, 2)
	go func() { // local -> daemon
		buf := make([]byte, 32*1024)
		for {
			n, rerr := in.Read(buf)
			if n > 0 {
				if serr := stream.Send(&unwedgev1.TunnelChunk{Data: buf[:n]}); serr != nil {
					errc <- serr
					return
				}
			}
			if rerr != nil {
				// Half-close our send side; keep receiving until the daemon
				// closes the stream (authoritative end of the connection).
				_ = stream.CloseSend()
				if rerr != io.EOF {
					errc <- rerr
				}
				return
			}
		}
	}()
	go func() { // daemon -> local
		for {
			msg, rerr := stream.Recv()
			if rerr != nil {
				errc <- rerr
				return
			}
			if b := msg.GetData(); len(b) > 0 {
				if _, werr := out.Write(b); werr != nil {
					errc <- werr
					return
				}
			}
		}
	}()
	if err := <-errc; err != nil && err != io.EOF {
		return err
	}
	return nil
}

// SCPUploadFile copies localPath to remotePath on the target over the classic
// scp protocol and returns the number of bytes written.
func (c *Client) SCPUploadFile(ctx context.Context, localPath, remotePath, hostOverride string, timeout time.Duration) (int64, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return 0, fmt.Errorf("client: open %s: %w", localPath, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if fi.IsDir() {
		return 0, fmt.Errorf("client: %s is a directory", localPath)
	}
	stream, err := c.API.SCPUpload(ctx)
	if err != nil {
		return 0, err
	}
	if err := stream.Send(&unwedgev1.SCPUploadRequest{
		Payload: &unwedgev1.SCPUploadRequest_Metadata_{
			Metadata: &unwedgev1.SCPUploadRequest_Metadata{
				RemotePath:   remotePath,
				Size:         fi.Size(),
				Mode:         uint32(fi.Mode().Perm()),
				TimeoutMs:    timeout.Milliseconds(),
				HostOverride: hostOverride,
			},
		},
	}); err != nil {
		return 0, err
	}
	buf := make([]byte, 64*1024)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&unwedgev1.SCPUploadRequest{
				Payload: &unwedgev1.SCPUploadRequest_Chunk{Chunk: buf[:n]},
			}); err != nil {
				return 0, err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return 0, fmt.Errorf("client: read %s: %w", localPath, rerr)
		}
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return 0, err
	}
	return resp.GetBytesWritten(), nil
}

// SCPDownloadFile copies remotePath from the target to localPath over the
// classic scp protocol and returns the number of bytes written. The local file
// is created with the mode the target reports.
func (c *Client) SCPDownloadFile(ctx context.Context, remotePath, localPath, hostOverride string, timeout time.Duration) (int64, error) {
	stream, err := c.API.SCPDownload(ctx, &unwedgev1.SCPDownloadRequest{
		RemotePath:   remotePath,
		TimeoutMs:    timeout.Milliseconds(),
		HostOverride: hostOverride,
	})
	if err != nil {
		return 0, err
	}
	msg, err := stream.Recv()
	if err != nil {
		return 0, err
	}
	meta := msg.GetMetadata()
	if meta == nil {
		return 0, fmt.Errorf("client: expected scp metadata as the first message")
	}
	mode := os.FileMode(meta.GetMode()).Perm()
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(localPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return 0, fmt.Errorf("client: create %s: %w", localPath, err)
	}
	defer f.Close()
	var written int64
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return written, err
		}
		if b := msg.GetChunk(); len(b) > 0 {
			n, werr := f.Write(b)
			written += int64(n)
			if werr != nil {
				return written, fmt.Errorf("client: write %s: %w", localPath, werr)
			}
		}
	}
	return written, f.Close()
}

type bootStream interface {
	Recv() (*unwedgev1.BootEvent, error)
}

func drainBootEvents(stream bootStream, h BootEventHandler) error {
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h != nil {
			h(ev)
		}
	}
}
