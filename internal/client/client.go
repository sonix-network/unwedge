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

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	unwedgev1 "github.com/sonix-network/unwedge/gen/unwedge/v1"
	"github.com/sonix-network/unwedge/internal/tlsutil"
)

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
	conn, err := grpc.NewClient(o.Address, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("client: dial %s: %w", o.Address, err)
	}
	return &Client{conn: conn, API: unwedgev1.NewUnwedgeClient(conn)}, nil
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
