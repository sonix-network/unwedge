package server_test

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	unwedgev1 "github.com/sonix-network/unwedge/gen/unwedge/v1"
	"github.com/sonix-network/unwedge/internal/client"
	"github.com/sonix-network/unwedge/internal/power"
	"github.com/sonix-network/unwedge/internal/serialconsole"
	"github.com/sonix-network/unwedge/internal/server"
	"github.com/sonix-network/unwedge/internal/session"
)

// nullConn is a serial transport that never produces data and swallows writes.
type nullConn struct{ done chan struct{} }

func newNullConn() *nullConn                    { return &nullConn{done: make(chan struct{})} }
func (c *nullConn) Read(p []byte) (int, error)  { <-c.done; return 0, io.EOF }
func (c *nullConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *nullConn) Close() error                { close(c.done); return nil }

func startLocked(t *testing.T, ttl time.Duration) func() *client.Client {
	t.Helper()
	mgr := session.NewManager(ttl)
	con := serialconsole.New(newNullConn(), 4096)
	t.Cleanup(func() { con.Close() })
	svc := server.New(server.Deps{
		Version:  "t",
		Console:  con,
		Power:    power.NewFake(power.StateOn),
		Sessions: mgr,
	})
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(svc.UnaryInterceptor),
		grpc.ChainStreamInterceptor(svc.StreamInterceptor),
	)
	unwedgev1.RegisterUnwedgeServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	t.Cleanup(mgr.Close)

	return func() *client.Client {
		cl, err := client.Dial(client.Options{
			Address: "passthrough:///bufnet",
			Dialer:  func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) },
		})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { cl.Close() })
		return cl
	}
}

func powerStatus(ctx context.Context, cl *client.Client) error {
	_, err := cl.API.PowerControl(ctx, &unwedgev1.PowerControlRequest{
		Action: unwedgev1.PowerAction_POWER_ACTION_STATUS,
	})
	return err
}

func TestOperationalRequiresSession(t *testing.T) {
	dial := startLocked(t, time.Minute)
	cl := dial()
	ctx := context.Background()

	// Without a session, an operational RPC is refused.
	if err := powerStatus(ctx, cl); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition without session, got %v", err)
	}
	// GetStatus is exempt and works without a session.
	if _, err := cl.API.GetStatus(ctx, &unwedgev1.GetStatusRequest{}); err != nil {
		t.Fatalf("GetStatus should be exempt: %v", err)
	}
	// After acquiring, it works.
	if _, err := cl.Acquire(ctx, "a", -1); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := powerStatus(ctx, cl); err != nil {
		t.Fatalf("PowerControl with session: %v", err)
	}
}

func TestMutualExclusion(t *testing.T) {
	dial := startLocked(t, time.Minute)
	ctx := context.Background()
	a, b := dial(), dial()

	if _, err := a.Acquire(ctx, "a", -1); err != nil {
		t.Fatalf("a.Acquire: %v", err)
	}
	// b cannot acquire while a holds it (fail-fast).
	if _, err := b.Acquire(ctx, "b", -1); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for b, got %v", err)
	}
	// a releases; b can now acquire.
	if err := a.Release(ctx); err != nil {
		t.Fatalf("a.Release: %v", err)
	}
	if _, err := b.Acquire(ctx, "b", -1); err != nil {
		t.Fatalf("b.Acquire after release: %v", err)
	}
	if err := powerStatus(ctx, b); err != nil {
		t.Fatalf("b operational after acquire: %v", err)
	}
	// a's stale session no longer works.
	if err := powerStatus(ctx, a); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a's released session should be rejected, got %v", err)
	}
}

func TestSessionExpiryFrees(t *testing.T) {
	dial := startLocked(t, 120*time.Millisecond)
	ctx := context.Background()
	a, b := dial(), dial()

	if _, err := a.Acquire(ctx, "a", -1); err != nil {
		t.Fatalf("a.Acquire: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // idle past TTL

	// b acquires because a's lock expired.
	if _, err := b.Acquire(ctx, "b", -1); err != nil {
		t.Fatalf("b.Acquire after a expired: %v", err)
	}
	// a's now-lost session is rejected on the next operational call.
	if err := powerStatus(ctx, a); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expired session should be rejected, got %v", err)
	}
}

func TestBlockingAcquireWakesOnRelease(t *testing.T) {
	dial := startLocked(t, time.Minute)
	ctx := context.Background()
	a, b := dial(), dial()

	if _, err := a.Acquire(ctx, "a", -1); err != nil {
		t.Fatalf("a.Acquire: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := b.Acquire(ctx, "b", 0) // block until free
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("b.Acquire returned while a held the lock")
	case <-time.After(100 * time.Millisecond):
	}
	if err := a.Release(ctx); err != nil {
		t.Fatalf("a.Release: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("b.Acquire after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("b.Acquire did not wake after release")
	}
}

func TestConsoleReadIsLockFree(t *testing.T) {
	dial := startLocked(t, time.Minute)
	ctx := context.Background()
	holder, observer := dial(), dial()

	if _, err := holder.Acquire(ctx, "holder", -1); err != nil {
		t.Fatalf("holder.Acquire: %v", err)
	}
	// An observer without a session can read/stream the console while the lock
	// is held by someone else.
	if _, err := observer.API.ReadConsoleLog(ctx, &unwedgev1.ReadConsoleLogRequest{}); err != nil {
		t.Fatalf("ReadConsoleLog should be lock-free: %v", err)
	}
	stream, err := observer.API.StreamConsole(ctx, &unwedgev1.StreamConsoleRequest{})
	if err != nil {
		t.Fatalf("StreamConsole open should be lock-free: %v", err)
	}
	_ = stream
	// But writing to the console still requires the lock.
	if _, err := observer.API.WriteConsole(ctx, &unwedgev1.WriteConsoleRequest{Data: []byte("x")}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("WriteConsole without lock should be refused, got %v", err)
	}
}

func TestGetStatusReportsLock(t *testing.T) {
	dial := startLocked(t, time.Minute)
	ctx := context.Background()
	a, observer := dial(), dial()

	if _, err := a.Acquire(ctx, "owner-x", -1); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	st, err := observer.API.GetStatus(ctx, &unwedgev1.GetStatusRequest{})
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !st.SessionActive || st.SessionOwner != "owner-x" {
		t.Fatalf("status lock = active:%v owner:%q", st.SessionActive, st.SessionOwner)
	}
}
