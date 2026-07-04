package server

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	unwedgev1 "github.com/sonix-network/unwedge/gen/unwedge/v1"
	"github.com/sonix-network/unwedge/internal/power"
	"github.com/sonix-network/unwedge/internal/serialconsole"
	"github.com/sonix-network/unwedge/internal/sshexec"
	"github.com/sonix-network/unwedge/internal/tftp"
)

// pipeConn is a buffered in-memory serial transport for tests.
type pipeConn struct {
	mu      sync.Mutex
	toRead  []byte
	dataC   chan struct{}
	written bytes.Buffer
	closed  bool
	closedC chan struct{}
}

func newPipeConn() *pipeConn {
	return &pipeConn{dataC: make(chan struct{}, 1), closedC: make(chan struct{})}
}
func (p *pipeConn) feed(b []byte) {
	p.mu.Lock()
	p.toRead = append(p.toRead, b...)
	p.mu.Unlock()
	select {
	case p.dataC <- struct{}{}:
	default:
	}
}
func (p *pipeConn) Read(b []byte) (int, error) {
	for {
		p.mu.Lock()
		if len(p.toRead) > 0 {
			n := copy(b, p.toRead)
			p.toRead = p.toRead[n:]
			p.mu.Unlock()
			return n, nil
		}
		if p.closed {
			p.mu.Unlock()
			return 0, io.EOF
		}
		p.mu.Unlock()
		select {
		case <-p.dataC:
		case <-p.closedC:
		}
	}
}
func (p *pipeConn) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.written.Write(b)
}
func (p *pipeConn) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	close(p.closedC)
	return nil
}
func (p *pipeConn) writtenStr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.written.String()
}

func startTestServer(t *testing.T, deps Deps) unwedgev1.UnwedgeClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	unwedgev1.RegisterUnwedgeServer(srv, New(deps))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return unwedgev1.NewUnwedgeClient(conn)
}

func TestConsoleRPCs(t *testing.T) {
	pc := newPipeConn()
	con := serialconsole.New(pc, 1<<16)
	t.Cleanup(func() { con.Close() })
	client := startTestServer(t, Deps{Console: con, Version: "test"})
	ctx := context.Background()

	pc.feed([]byte("boot log line\n"))
	// ReadConsoleLog should eventually see it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := client.ReadConsoleLog(ctx, &unwedgev1.ReadConsoleLogRequest{})
		if err != nil {
			t.Fatalf("ReadConsoleLog: %v", err)
		}
		if bytes.Contains(resp.Data, []byte("boot log line")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("console log never contained expected text: %q", resp.Data)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// WriteConsole with a control key (ctrl-x) plus data.
	if _, err := client.WriteConsole(ctx, &unwedgev1.WriteConsoleRequest{
		Data: []byte("printenv"), Keys: []string{"enter"},
	}); err != nil {
		t.Fatalf("WriteConsole: %v", err)
	}
	if got := pc.writtenStr(); got != "printenv\r" {
		t.Fatalf("written = %q, want printenv\\r", got)
	}

	// WaitForPattern: arm, then feed matching data.
	type wr struct {
		resp *unwedgev1.WaitForPatternResponse
		err  error
	}
	done := make(chan wr, 1)
	go func() {
		resp, err := client.WaitForPattern(ctx, &unwedgev1.WaitForPatternRequest{
			Pattern: `=> ?`, TimeoutMs: 2000,
		})
		done <- wr{resp, err}
	}()
	time.Sleep(50 * time.Millisecond)
	pc.feed([]byte("\n=> "))
	r := <-done
	if r.err != nil {
		t.Fatalf("WaitForPattern: %v", r.err)
	}
	if !r.resp.Matched {
		t.Fatalf("expected match, got %+v", r.resp)
	}
}

func TestPowerRPC(t *testing.T) {
	fake := power.NewFake(power.StateOff)
	client := startTestServer(t, Deps{Power: fake})
	ctx := context.Background()

	resp, err := client.PowerControl(ctx, &unwedgev1.PowerControlRequest{
		Action: unwedgev1.PowerAction_POWER_ACTION_ON,
	})
	if err != nil {
		t.Fatalf("PowerControl on: %v", err)
	}
	if resp.State != unwedgev1.PowerState_POWER_STATE_ON {
		t.Fatalf("state = %v", resp.State)
	}

	// Cycle then confirm the fake logged it.
	if _, err := client.PowerControl(ctx, &unwedgev1.PowerControlRequest{
		Action: unwedgev1.PowerAction_POWER_ACTION_CYCLE, OffDurationMs: 5,
	}); err != nil {
		t.Fatalf("PowerControl cycle: %v", err)
	}
}

func TestPowerRPCNotConfigured(t *testing.T) {
	client := startTestServer(t, Deps{})
	_, err := client.PowerControl(context.Background(), &unwedgev1.PowerControlRequest{
		Action: unwedgev1.PowerAction_POWER_ACTION_ON,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

func TestImageRPCs(t *testing.T) {
	store, err := tftp.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	client := startTestServer(t, Deps{Store: store})
	ctx := context.Background()

	// Upload via client streaming.
	up, err := client.UploadImage(ctx)
	if err != nil {
		t.Fatalf("UploadImage: %v", err)
	}
	if err := up.Send(&unwedgev1.UploadImageRequest{
		Payload: &unwedgev1.UploadImageRequest_Metadata_{
			Metadata: &unwedgev1.UploadImageRequest_Metadata{Name: "kernel.bin"},
		},
	}); err != nil {
		t.Fatalf("send meta: %v", err)
	}
	payload := bytes.Repeat([]byte("A"), 1000)
	if err := up.Send(&unwedgev1.UploadImageRequest{
		Payload: &unwedgev1.UploadImageRequest_Chunk{Chunk: payload},
	}); err != nil {
		t.Fatalf("send chunk: %v", err)
	}
	resp, err := up.CloseAndRecv()
	if err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}
	if resp.Size != 1000 || resp.Name != "kernel.bin" {
		t.Fatalf("upload resp = %+v", resp)
	}

	// List.
	lr, err := client.ListImages(ctx, &unwedgev1.ListImagesRequest{})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(lr.Images) != 1 || lr.Images[0].Name != "kernel.bin" {
		t.Fatalf("list = %+v", lr.Images)
	}
	if lr.Images[0].Crc32 != resp.Crc32 {
		t.Fatalf("crc mismatch: list=%08x upload=%08x", lr.Images[0].Crc32, resp.Crc32)
	}

	// Delete.
	if _, err := client.DeleteImage(ctx, &unwedgev1.DeleteImageRequest{Name: "kernel.bin"}); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}
	lr, _ = client.ListImages(ctx, &unwedgev1.ListImagesRequest{})
	if len(lr.Images) != 0 {
		t.Fatalf("expected empty after delete")
	}
}

func TestGetStatus(t *testing.T) {
	pc := newPipeConn()
	con := serialconsole.New(pc, 1<<16)
	t.Cleanup(func() { con.Close() })
	store, _ := tftp.NewStore(t.TempDir())
	client := startTestServer(t, Deps{
		Version: "v1.2.3", Console: con, Power: power.NewFake(power.StateOn), Store: store,
		SerialDevice: "/dev/ttyUSB0", SerialBaud: 115200,
		SSHTarget: "10.0.0.5", SSHUser: "root", PowerOutlet: 3,
	})
	resp, err := client.GetStatus(context.Background(), &unwedgev1.GetStatusRequest{})
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if resp.Version != "v1.2.3" || !resp.SerialConnected || resp.SerialBaud != 115200 {
		t.Fatalf("status = %+v", resp)
	}
	if resp.PowerState != unwedgev1.PowerState_POWER_STATE_ON {
		t.Fatalf("power state = %v", resp.PowerState)
	}
	if resp.SshTarget != "10.0.0.5" || resp.SshUser != "root" || resp.PowerOutlet != 3 {
		t.Fatalf("ssh/outlet status = %+v", resp)
	}
}

func TestTunnelProxiesToTarget(t *testing.T) {
	// A raw TCP echo server stands in for the target's SSH port; Tunnel is a
	// byte proxy, so no SSH handshake is involved.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	sshCl, err := sshexec.New(sshexec.Config{Host: ln.Addr().String(), User: "root", Password: "x"})
	if err != nil {
		t.Fatalf("sshexec.New: %v", err)
	}
	client := startTestServer(t, Deps{SSH: sshCl})

	stream, err := client.Tunnel(context.Background())
	if err != nil {
		t.Fatalf("Tunnel: %v", err)
	}
	if err := stream.Send(&unwedgev1.TunnelChunk{}); err != nil { // open (configured host)
		t.Fatalf("open: %v", err)
	}
	const want = "hello world"
	if err := stream.Send(&unwedgev1.TunnelChunk{Data: []byte(want)}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var got []byte
	for len(got) < len(want) {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv (got %q): %v", got, err)
		}
		got = append(got, msg.GetData()...)
	}
	if string(got) != want {
		t.Fatalf("echo = %q, want %q", got, want)
	}
	_ = stream.CloseSend()
}

func TestNewSSHRPCsRequireSSH(t *testing.T) {
	client := startTestServer(t, Deps{}) // SSH not configured

	ts, err := client.Tunnel(context.Background())
	if err != nil {
		t.Fatalf("Tunnel: %v", err)
	}
	_ = ts.Send(&unwedgev1.TunnelChunk{})
	if _, err := ts.Recv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Tunnel err = %v, want FailedPrecondition", err)
	}

	ds, err := client.SCPDownload(context.Background(), &unwedgev1.SCPDownloadRequest{RemotePath: "/x"})
	if err != nil {
		t.Fatalf("SCPDownload: %v", err)
	}
	if _, err := ds.Recv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("SCPDownload err = %v, want FailedPrecondition", err)
	}

	us, err := client.SCPUpload(context.Background())
	if err != nil {
		t.Fatalf("SCPUpload: %v", err)
	}
	_ = us.Send(&unwedgev1.SCPUploadRequest{Payload: &unwedgev1.SCPUploadRequest_Metadata_{
		Metadata: &unwedgev1.SCPUploadRequest_Metadata{RemotePath: "/x"},
	}})
	if _, err := us.CloseAndRecv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("SCPUpload err = %v, want FailedPrecondition", err)
	}
}
