// Package uboot orchestrates the vEdge 1000's U-Boot bootloader over the serial
// console: interrupting the autoboot countdown, running commands at the prompt,
// and performing the full TFTP netboot flow used for kernel/OS development.
//
// Reference netboot recipe (from the OpenWrt device wiki, Octeon CN6130):
//
//	setenv ethact octmgmt0
//	dhcp
//	setenv serverip <controller-ip>
//	tftpboot $loadaddr $serverip:/<image>
//	crc32 -v <loadaddr> <len> <crc>      (optional integrity check)
//	bootoctlinux $loadaddr coremask=f endbootargs [extra args]
package uboot

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sonix-network/unwedge/internal/power"
	"github.com/sonix-network/unwedge/internal/serialconsole"
)

// Config parameterizes the orchestration. Zero values fall back to vEdge 1000
// defaults via withDefaults.
type Config struct {
	// PromptPattern matches the U-Boot prompt (default `=> ?$` per line).
	PromptPattern string
	// InterruptPattern matches the autoboot line inviting a key press.
	InterruptPattern string
	// InterruptKey is the console key that stops autoboot (Ctrl-X here).
	InterruptKey string
	// EthAct is the U-Boot network device used for netbooting (octmgmt0).
	EthAct string
	// LoadAddr is where the image is loaded in RAM.
	LoadAddr string
	// CoreMask passed to bootoctlinux (f = all 4 CN6130 cores).
	CoreMask string
	// ServerIP is the controller's IP the target TFTPs the image from. If empty
	// at Netboot time it must be supplied by the caller.
	ServerIP string
	// CommandTimeout bounds a single U-Boot command awaiting the prompt.
	CommandTimeout time.Duration
	// KernelBanner matches early kernel output confirming the image booted.
	KernelBanner string
}

func withDefaults(c Config) Config {
	if c.PromptPattern == "" {
		c.PromptPattern = `=> ?$`
	}
	if c.InterruptPattern == "" {
		c.InterruptPattern = `Hit ctrl-x to stop booting`
	}
	if c.InterruptKey == "" {
		c.InterruptKey = "ctrl-x"
	}
	if c.EthAct == "" {
		c.EthAct = "octmgmt0"
	}
	if c.LoadAddr == "" {
		c.LoadAddr = "0x20000000"
	}
	if c.CoreMask == "" {
		c.CoreMask = "f"
	}
	if c.CommandTimeout == 0 {
		c.CommandTimeout = 30 * time.Second
	}
	if c.KernelBanner == "" {
		// Matches either the U-Boot ELF loader line or the kernel banner.
		c.KernelBanner = `(Linux version|Loading big-endian Linux kernel|Starting cores)`
	}
	return c
}

// EventKind classifies a progress Event.
type EventKind int

const (
	EventInfo EventKind = iota
	EventConsole
	EventStage
	EventSuccess
	EventError
)

// Event is a progress update emitted during an operation.
type Event struct {
	Kind    EventKind
	Stage   string
	Message string
	Console []byte
}

// Emit receives progress events. It must be safe to call from the caller's
// goroutine and must not block indefinitely.
type Emit func(Event)

func (e Emit) info(stage, format string, args ...interface{}) {
	if e != nil {
		e(Event{Kind: EventInfo, Stage: stage, Message: fmt.Sprintf(format, args...)})
	}
}

func (e Emit) stage(stage, msg string) {
	if e != nil {
		e(Event{Kind: EventStage, Stage: stage, Message: msg})
	}
}

func (e Emit) console(stage string, b []byte) {
	if e != nil && len(b) > 0 {
		e(Event{Kind: EventConsole, Stage: stage, Console: append([]byte(nil), b...)})
	}
}

// Orchestrator drives U-Boot over a serial console with optional power control.
type Orchestrator struct {
	console *serialconsole.Console
	power   power.Controller // may be nil if power control is unavailable
	cfg     Config

	promptRE    *regexp.Regexp
	interruptRE *regexp.Regexp
	bannerRE    *regexp.Regexp
	interruptKB []byte
}

// New builds an Orchestrator. power may be nil (power-cycle requests then error).
func New(console *serialconsole.Console, pwr power.Controller, cfg Config) (*Orchestrator, error) {
	cfg = withDefaults(cfg)
	promptRE, err := regexp.Compile(cfg.PromptPattern)
	if err != nil {
		return nil, fmt.Errorf("uboot: bad prompt pattern: %w", err)
	}
	interruptRE, err := regexp.Compile(cfg.InterruptPattern)
	if err != nil {
		return nil, fmt.Errorf("uboot: bad interrupt pattern: %w", err)
	}
	bannerRE, err := regexp.Compile(cfg.KernelBanner)
	if err != nil {
		return nil, fmt.Errorf("uboot: bad kernel banner pattern: %w", err)
	}
	kb, err := serialconsole.KeyBytes(cfg.InterruptKey)
	if err != nil {
		return nil, fmt.Errorf("uboot: bad interrupt key: %w", err)
	}
	return &Orchestrator{
		console:     console,
		power:       pwr,
		cfg:         cfg,
		promptRE:    promptRE,
		interruptRE: interruptRE,
		bannerRE:    bannerRE,
		interruptKB: kb,
	}, nil
}

// Config returns the effective configuration (with defaults applied).
func (o *Orchestrator) Config() Config { return o.cfg }

// sendAndExpect subscribes first, then writes toSend, then accumulates console
// output until re matches or ctx is done. Subscribing before writing guarantees
// output produced by the write is not missed.
func (o *Orchestrator) sendAndExpect(ctx context.Context, toSend []byte, re *regexp.Regexp, emit Emit, stage string) ([]byte, bool, error) {
	sub := o.console.Subscribe(0)
	defer sub.Close()
	if len(toSend) > 0 {
		if _, err := o.console.Write(toSend); err != nil {
			return nil, false, fmt.Errorf("uboot: write: %w", err)
		}
	}
	var acc []byte
	for {
		select {
		case <-ctx.Done():
			return acc, false, ctx.Err()
		case chunk, ok := <-sub.C():
			if !ok {
				return acc, false, serialconsole.ErrClosed
			}
			emit.console(stage, chunk)
			acc = append(acc, chunk...)
			if re.Match(acc) {
				return acc, true, nil
			}
			// Bound memory on very chatty boots.
			if len(acc) > 1<<20 {
				acc = append([]byte(nil), acc[len(acc)-(1<<19):]...)
			}
		}
	}
}

// InterruptBoot (optionally) power-cycles the target, then catches the autoboot
// prompt and stops it, leaving U-Boot at its command prompt.
func (o *Orchestrator) InterruptBoot(ctx context.Context, powerCycle bool, emit Emit) error {
	if powerCycle {
		if o.power == nil {
			return fmt.Errorf("uboot: power cycle requested but no power controller configured")
		}
		emit.stage("power_cycle", "power-cycling target")
		if err := o.power.Cycle(ctx, 0); err != nil {
			return fmt.Errorf("uboot: power cycle: %w", err)
		}
	}

	// Watch for the autoboot invitation. We subscribe here (after the power
	// cycle) which is fine: the invitation appears seconds into the boot.
	emit.stage("await_interrupt", "waiting for U-Boot autoboot prompt")
	if _, _, err := o.console.WaitForPattern(ctx, o.interruptRE, 0); err != nil {
		return fmt.Errorf("uboot: never saw autoboot prompt: %w", err)
	}
	emit.info("await_interrupt", "autoboot prompt seen; sending interrupt key %q", o.cfg.InterruptKey)

	// Spam the interrupt key a few times while looking for the prompt; timing is
	// tight and an extra control byte at the prompt is harmless.
	for attempt := 0; attempt < 8; attempt++ {
		if _, err := o.console.Write(o.interruptKB); err != nil {
			return fmt.Errorf("uboot: send interrupt: %w", err)
		}
		cctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
		_, _, err := o.console.WaitForPattern(cctx, o.promptRE, 256)
		cancel()
		if err == nil {
			emit.stage("prompt", "reached U-Boot prompt")
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return fmt.Errorf("uboot: sent interrupt key but never reached prompt")
}

// RunCommand sends a single command and returns the output captured up to the
// returning prompt. It assumes U-Boot is already at the prompt.
func (o *Orchestrator) RunCommand(ctx context.Context, command string) (string, error) {
	if o.cfg.CommandTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.cfg.CommandTimeout)
		defer cancel()
	}
	acc, ok, err := o.sendAndExpect(ctx, []byte(command+"\r"), o.promptRE, nil, "cmd")
	out := cleanCommandOutput(string(acc), command)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, fmt.Errorf("uboot: prompt did not return after %q", command)
	}
	return out, nil
}

// runChecked runs a command and fails if the output contains an error marker.
func (o *Orchestrator) runChecked(ctx context.Context, emit Emit, stage, command string, errMarkers ...string) (string, error) {
	emit.info(stage, "uboot> %s", command)
	out, err := o.RunCommand(ctx, command)
	emit.console(stage, []byte(out))
	if err != nil {
		return out, err
	}
	low := strings.ToLower(out)
	for _, m := range errMarkers {
		if strings.Contains(low, strings.ToLower(m)) {
			return out, fmt.Errorf("uboot: %q reported error (matched %q)", command, m)
		}
	}
	return out, nil
}

// NetbootParams configures a single Netboot invocation.
type NetbootParams struct {
	Image       string // filename served by the controller's TFTP server
	PowerCycle  bool   // interrupt boot first (power-cycling the board)
	ServerIP    string // controller IP; overrides Config.ServerIP if set
	KernelArgs  string // extra args appended to bootoctlinux
	VerifyCRC32 bool
	ImageLen    int    // image byte length, required if VerifyCRC32
	ImageCRC32  uint32 // expected IEEE CRC32, required if VerifyCRC32
	Timeout     time.Duration
}

// Netboot performs the full TFTP kernel boot flow, emitting progress.
func (o *Orchestrator) Netboot(ctx context.Context, p NetbootParams, emit Emit) error {
	if p.Image == "" {
		return fmt.Errorf("uboot: netboot image name required")
	}
	serverIP := p.ServerIP
	if serverIP == "" {
		serverIP = o.cfg.ServerIP
	}
	if serverIP == "" {
		return fmt.Errorf("uboot: netboot serverip required (controller IP reachable from target)")
	}
	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}

	if p.PowerCycle {
		if err := o.InterruptBoot(ctx, true, emit); err != nil {
			return err
		}
	}

	// Select the management NIC and get an address via DHCP.
	if _, err := o.runChecked(ctx, emit, "net", "setenv ethact "+o.cfg.EthAct, "Unknown command"); err != nil {
		return err
	}
	if _, err := o.runChecked(ctx, emit, "net", "dhcp", "TIMEOUT", "not set", "failed"); err != nil {
		return err
	}
	if _, err := o.runChecked(ctx, emit, "net", fmt.Sprintf("setenv serverip %s", serverIP)); err != nil {
		return err
	}
	// Force loadaddr so $loadaddr and the numeric address agree.
	if _, err := o.runChecked(ctx, emit, "net", "setenv loadaddr "+o.cfg.LoadAddr); err != nil {
		return err
	}

	// TFTP the image.
	emit.stage("tftp", "fetching image over TFTP")
	tftpCmd := fmt.Sprintf("tftpboot $loadaddr $serverip:/%s", p.Image)
	if _, err := o.runChecked(ctx, emit, "tftp", tftpCmd,
		"TFTP error", "Retry count exceeded", "not set", "abort"); err != nil {
		return err
	}

	// Optional CRC32 integrity verification.
	if p.VerifyCRC32 {
		emit.stage("verify", "verifying CRC32 of loaded image")
		crcCmd := fmt.Sprintf("crc32 -v %s 0x%x %08x", o.cfg.LoadAddr, p.ImageLen, p.ImageCRC32)
		if _, err := o.runChecked(ctx, emit, "verify", crcCmd, "bad", "!=", "error"); err != nil {
			return err
		}
	}

	// Boot the kernel. bootoctlinux is terminal: the prompt will not return, so
	// instead of RunCommand we send the command and wait for the kernel banner.
	//
	// panic=0 makes the kernel halt (and stay) on panic instead of rebooting.
	// Without it, a panic — e.g. a bad/oversized initramfs failing to unpack —
	// reboots the board, U-Boot autoboots whatever is in flash, and the netbooted
	// image is silently replaced by the on-flash one. That turns a failed boot into
	// a false "success" (the smoke test sees the flashed image come up cleanly).
	// Keeping the panic on the console makes it visible and lets the smoke test's
	// "Kernel panic" failure marker fire. Callers can override via KernelArgs, where
	// a later panic= wins.
	bootCmd := fmt.Sprintf("bootoctlinux $loadaddr coremask=%s endbootargs panic=0", o.cfg.CoreMask)
	if strings.TrimSpace(p.KernelArgs) != "" {
		bootCmd += " " + strings.TrimSpace(p.KernelArgs)
	}
	emit.stage("boot", "booting kernel: "+bootCmd)
	_, ok, err := o.sendAndExpect(ctx, []byte(bootCmd+"\r"), o.bannerRE, emit, "boot")
	if err != nil {
		return fmt.Errorf("uboot: booting kernel: %w", err)
	}
	if !ok {
		return fmt.Errorf("uboot: kernel banner not seen after bootoctlinux")
	}
	emit.stage("boot", "kernel is booting")
	return nil
}

// cleanCommandOutput strips the echoed command line and the trailing prompt from
// captured U-Boot output, returning just the command's response text.
func cleanCommandOutput(raw, command string) string {
	s := raw
	// Drop a leading echo of the command (U-Boot echoes typed input).
	if i := strings.Index(s, command); i >= 0 {
		s = s[i+len(command):]
	}
	s = strings.TrimLeft(s, "\r\n")
	// Drop a trailing prompt if present.
	lines := strings.Split(s, "\n")
	for len(lines) > 0 {
		last := strings.TrimRight(lines[len(lines)-1], "\r \t")
		if last == "=>" || last == "" {
			lines = lines[:len(lines)-1]
			continue
		}
		break
	}
	return strings.Join(lines, "\n")
}
