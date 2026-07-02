package uboot

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sonix-network/unwedge/internal/power"
	"github.com/sonix-network/unwedge/internal/serialconsole"
)

// ubootEmu is a tiny stateful U-Boot emulator used as a serial transport. It
// models the autoboot countdown, the Ctrl-X interrupt, the prompt, and canned
// responses to the netboot command sequence.
type ubootEmu struct {
	mu      sync.Mutex
	out     []byte
	dataC   chan struct{}
	closedC chan struct{}
	closed  bool
	inbuf   []byte
	mode    string // "idle", "booting", "prompt", "kernel"
	booted  bool
}

func newUbootEmu() *ubootEmu {
	return &ubootEmu{dataC: make(chan struct{}, 1), closedC: make(chan struct{}), mode: "idle"}
}

func (e *ubootEmu) emit(s string) {
	e.mu.Lock()
	e.out = append(e.out, []byte(s)...)
	e.mu.Unlock()
	select {
	case e.dataC <- struct{}{}:
	default:
	}
}

// boot models a power-on: after a short delay it prints the banner and the
// autoboot invitation, then waits for Ctrl-X.
func (e *ubootEmu) boot() {
	e.mu.Lock()
	e.mode = "booting"
	e.mu.Unlock()
	go func() {
		time.Sleep(20 * time.Millisecond)
		e.emit("\n\nU-Boot 2013.07-g1874683 (Build time: Nov 05 2018)\n" +
			"DRAM: 4 GiB\nNet:   octmgmt0, octeth0\n" +
			"Hit ctrl-x to stop booting 0 \n")
	}()
}

func (e *ubootEmu) Read(p []byte) (int, error) {
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

func (e *ubootEmu) Write(p []byte) (int, error) {
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
				e.handleCommand(line)
			}
			e.mu.Lock()
		}
	}
	e.mu.Unlock()
	return len(p), nil
}

func (e *ubootEmu) handleCommand(line string) {
	// Echo the command as a terminal would.
	e.emit(line + "\r\n")
	switch {
	case strings.HasPrefix(line, "setenv"):
		e.emit("=> ")
	case line == "dhcp":
		e.emit("BOOTP broadcast 1\nDHCP client bound to address 10.1.2.34\n=> ")
	case strings.HasPrefix(line, "tftpboot"):
		e.emit("Using octmgmt0 device\nTFTP from server 10.1.2.3; our IP address is 10.1.2.34\n" +
			"Load address: 0x20000000\nLoading: #################\n\t 5 MiB/s\ndone\n" +
			"Bytes transferred = 11052832 (a8b4e0 hex)\n=> ")
	case strings.HasPrefix(line, "crc32"):
		e.emit("crc32 for 20000000 ... == a8b4e0\n=> ")
	case strings.HasPrefix(line, "bootoctlinux"):
		e.mu.Lock()
		e.mode = "kernel"
		e.mu.Unlock()
		e.emit("argv[2]: endbootargs\n" +
			"## Loading big-endian Linux kernel with entry point: 0xffffffff81894af0 ...\n" +
			"Bootloader: Done loading app on coremask: 0x1\nStarting cores:\n 0x1\n" +
			"[    0.000000] Linux version 5.15.114\n")
	default:
		e.emit("Unknown command '" + line + "' - try 'help'\n=> ")
	}
}

func (e *ubootEmu) Close() error {
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

// emuPower is a power controller that reboots the emulator on cycle/on.
type emuPower struct {
	emu   *ubootEmu
	state power.State
}

func (p *emuPower) Status(context.Context) (power.State, error) { return p.state, nil }
func (p *emuPower) On(context.Context) error                    { p.state = power.StateOn; p.emu.boot(); return nil }
func (p *emuPower) Off(context.Context) error                   { p.state = power.StateOff; return nil }
func (p *emuPower) Cycle(ctx context.Context, d time.Duration) error {
	_ = p.Off(ctx)
	return p.On(ctx)
}
func (p *emuPower) Close() error { return nil }

func newTestOrchestrator(t *testing.T) (*Orchestrator, *ubootEmu, *emuPower) {
	t.Helper()
	emu := newUbootEmu()
	con := serialconsole.New(emu, 1<<20)
	t.Cleanup(func() { con.Close() })
	pwr := &emuPower{emu: emu, state: power.StateOn}
	o, err := New(con, pwr, Config{ServerIP: "10.1.2.3"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o, emu, pwr
}

func TestInterruptBoot(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stages []string
	emit := func(ev Event) {
		if ev.Kind == EventStage {
			stages = append(stages, ev.Stage)
		}
	}
	if err := o.InterruptBoot(ctx, true, emit); err != nil {
		t.Fatalf("InterruptBoot: %v", err)
	}
	if !contains(stages, "prompt") {
		t.Fatalf("expected to reach prompt stage, got %v", stages)
	}
}

func TestRunCommandAtPrompt(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := o.InterruptBoot(ctx, true, nil); err != nil {
		t.Fatalf("InterruptBoot: %v", err)
	}
	out, err := o.RunCommand(ctx, "dhcp")
	if err != nil {
		t.Fatalf("RunCommand dhcp: %v", err)
	}
	if !strings.Contains(out, "DHCP client bound") {
		t.Fatalf("unexpected dhcp output: %q", out)
	}
}

func TestNetbootFullFlow(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	var stages []string
	emit := func(ev Event) {
		if ev.Kind == EventStage {
			stages = append(stages, ev.Stage)
		}
	}
	err := o.Netboot(ctx, NetbootParams{
		Image:       "openwrt-initramfs-kernel.bin",
		PowerCycle:  true,
		VerifyCRC32: true,
		ImageLen:    11052832,
		ImageCRC32:  0xa8b4e0,
	}, emit)
	if err != nil {
		t.Fatalf("Netboot: %v", err)
	}
	for _, want := range []string{"power_cycle", "tftp", "verify", "boot"} {
		if !contains(stages, want) {
			t.Fatalf("missing stage %q in %v", want, stages)
		}
	}
}

func TestNetbootRequiresServerIP(t *testing.T) {
	emu := newUbootEmu()
	con := serialconsole.New(emu, 1<<20)
	defer con.Close()
	o, err := New(con, &emuPower{emu: emu}, Config{}) // no ServerIP
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = o.Netboot(context.Background(), NetbootParams{Image: "x.bin"}, nil)
	if err == nil || !strings.Contains(err.Error(), "serverip") {
		t.Fatalf("expected serverip error, got %v", err)
	}
}

func TestCleanCommandOutput(t *testing.T) {
	raw := "printenv\r\nbaudrate=115200\nethact=octmgmt0\n=> "
	got := cleanCommandOutput(raw, "printenv")
	want := "baudrate=115200\nethact=octmgmt0"
	if got != want {
		t.Fatalf("cleanCommandOutput = %q, want %q", got, want)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
