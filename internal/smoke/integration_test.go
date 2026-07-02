package smoke

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	unwedgev1 "github.com/sonix-network/unwedge/gen/unwedge/v1"
	"github.com/sonix-network/unwedge/internal/client"
	"github.com/sonix-network/unwedge/internal/power"
	"github.com/sonix-network/unwedge/internal/serialconsole"
	"github.com/sonix-network/unwedge/internal/server"
	"github.com/sonix-network/unwedge/internal/tftp"
	"github.com/sonix-network/unwedge/internal/uboot"
)

// boardEmu emulates a vEdge 1000: U-Boot autoboot + interrupt + netboot command
// responses, and then an OpenWrt boot that reaches a healthy console. It drives
// a full stack (serialconsole -> uboot orchestrator -> gRPC server) so the smoke
// engine can be exercised end to end.
type boardEmu struct {
	mu      sync.Mutex
	out     []byte
	dataC   chan struct{}
	closedC chan struct{}
	closed  bool
	inbuf   []byte
	mode    string
	panic   bool // when true, OpenWrt "boot" panics instead of coming up
}

func newBoardEmu(panicBoot bool) *boardEmu {
	return &boardEmu{dataC: make(chan struct{}, 1), closedC: make(chan struct{}), mode: "idle", panic: panicBoot}
}

func (e *boardEmu) emit(s string) {
	e.mu.Lock()
	e.out = append(e.out, []byte(s)...)
	e.mu.Unlock()
	select {
	case e.dataC <- struct{}{}:
	default:
	}
}

func (e *boardEmu) boot() {
	e.mu.Lock()
	e.mode = "booting"
	e.mu.Unlock()
	go func() {
		time.Sleep(20 * time.Millisecond)
		e.emit("\n\nU-Boot 2013.07\nDRAM: 4 GiB\nNet: octmgmt0\nHit ctrl-x to stop booting 0 \n")
	}()
}

func (e *boardEmu) Read(p []byte) (int, error) {
	for {
		e.mu.Lock()
		if len(e.out) > 0 {
			n := copy(p, e.out)
			e.out = e.out[n:]
			e.mu.Unlock()
			return n, nil
		}
		if e.closed {
			e.mu.Unlock()
			return 0, io.EOF
		}
		e.mu.Unlock()
		select {
		case <-e.dataC:
		case <-e.closedC:
		}
	}
}

func (e *boardEmu) Write(p []byte) (int, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	for _, b := range p {
		if b == 0x18 { // Ctrl-X
			if e.mode == "booting" {
				e.mode = "prompt"
				e.mu.Unlock()
				e.emit("\n=> ")
				e.mu.Lock()
			}
			continue
		}
		e.inbuf = append(e.inbuf, b)
		if b == '\r' || b == '\n' {
			line := strings.TrimRight(string(e.inbuf), "\r\n")
			e.inbuf = nil
			mode := e.mode
			e.mu.Unlock()
			if mode == "prompt" {
				e.handle(line)
			}
			e.mu.Lock()
		}
	}
	e.mu.Unlock()
	return len(p), nil
}

func (e *boardEmu) handle(line string) {
	e.emit(line + "\r\n")
	switch {
	case strings.HasPrefix(line, "setenv"):
		e.emit("=> ")
	case line == "dhcp":
		e.emit("DHCP client bound to address 10.1.2.34\n=> ")
	case strings.HasPrefix(line, "tftpboot"):
		e.emit("Using octmgmt0 device\nLoading: ####\ndone\nBytes transferred = 11052832 (a8b4e0 hex)\n=> ")
	case strings.HasPrefix(line, "crc32"):
		e.emit("crc32 ... == a8b4e0\n=> ")
	case strings.HasPrefix(line, "bootoctlinux"):
		e.mu.Lock()
		e.mode = "kernel"
		e.mu.Unlock()
		e.emit("## Loading big-endian Linux kernel ...\nStarting cores:\n 0x1\n[    0.000000] Linux version 5.15.114\n")
		go e.openwrtBoot()
	default:
		e.emit("Unknown command '" + line + "'\n=> ")
	}
}

func (e *boardEmu) openwrtBoot() {
	time.Sleep(80 * time.Millisecond)
	if e.panic {
		e.emit("[    2.5] Kernel panic - not syncing: VFS: Unable to mount root fs\n")
		return
	}
	e.emit("[   19.4] init: Console is alive\n" +
		"[   23.9] procd: - init -\n" +
		"BusyBox v1.36.1 built-in shell (ash)\n" +
		"Please press Enter to activate this console\n")
}

func (e *boardEmu) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	e.mu.Unlock()
	close(e.closedC)
	return nil
}

type emuPower struct {
	emu *boardEmu
	st  power.State
}

func (p *emuPower) Status(context.Context) (power.State, error) { return p.st, nil }
func (p *emuPower) On(context.Context) error                    { p.st = power.StateOn; p.emu.boot(); return nil }
func (p *emuPower) Off(context.Context) error                   { p.st = power.StateOff; return nil }
func (p *emuPower) Cycle(ctx context.Context, d time.Duration) error {
	_ = p.Off(ctx)
	return p.On(ctx)
}
func (p *emuPower) Close() error { return nil }

func buildStack(t *testing.T, panicBoot bool) *client.Client {
	t.Helper()
	emu := newBoardEmu(panicBoot)
	con := serialconsole.New(emu, 1<<20)
	t.Cleanup(func() { con.Close() })
	pwr := &emuPower{emu: emu, st: power.StateOn}
	orch, err := uboot.New(con, pwr, uboot.Config{ServerIP: "10.1.2.3"})
	if err != nil {
		t.Fatalf("uboot.New: %v", err)
	}
	store, err := tftp.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	svc := server.New(server.Deps{
		Version: "itest", Console: con, Power: pwr, Orchestrator: orch, Store: store,
	})

	lis := bufconn.Listen(1 << 20)
	gsrv := grpc.NewServer()
	unwedgev1.RegisterUnwedgeServer(gsrv, svc)
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(gsrv.Stop)

	cl, err := client.Dial(client.Options{
		Address: "passthrough:///bufnet",
		Dialer:  func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) },
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { cl.Close() })
	return cl
}

func writeTempImage(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/openwrt-cisco_vedge1000-initramfs-kernel.bin"
	if err := os.WriteFile(path, bytes.Repeat([]byte("K"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSmokeEndToEndPass drives the whole stack: upload -> netboot -> healthy boot.
func TestSmokeEndToEndPass(t *testing.T) {
	cl := buildStack(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := Run(ctx, cl, Config{
		ImagePath:   writeTempImage(t),
		VerifyCRC32: true,
		BootTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected PASS, got FAIL: %s\n--- boot log ---\n%s", res.Reason, res.BootLog)
	}
	if !bytes.Contains(res.BootLog, []byte("Please press Enter")) {
		t.Fatalf("boot log missing healthy marker:\n%s", res.BootLog)
	}
	if !bytes.Contains(res.BootLog, []byte("Hit ctrl-x")) {
		t.Fatalf("boot log should include the full boot from power-on:\n%s", res.BootLog)
	}
}

// TestSmokeEndToEndPanic verifies a kernel panic is detected as a failed boot.
func TestSmokeEndToEndPanic(t *testing.T) {
	cl := buildStack(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := Run(ctx, cl, Config{
		ImagePath:   writeTempImage(t),
		BootTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Success {
		t.Fatalf("expected FAIL on panic, got PASS")
	}
	if !strings.Contains(res.Reason, "boot failure") {
		t.Fatalf("reason = %q", res.Reason)
	}
}
