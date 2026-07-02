// Package smoke implements the release smoke test: netboot a freshly built
// OpenWrt image on the real vEdge 1000 and confirm it reaches a healthy
// userspace, capturing the full boot log as proof of a good boot. It is meant
// to gate the SONIX-network/openwrt weekly release build.
package smoke

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"

	unwedgev1 "github.com/sonix-network/unwedge/gen/unwedge/v1"
	"github.com/sonix-network/unwedge/internal/client"
)

// DefaultSuccessPattern matches OpenWrt reaching a usable console. The initramfs
// image used for smoke testing boots to RAM and prints this once userspace and
// procd are up.
const DefaultSuccessPattern = `Please press Enter to activate this console|BusyBox v[0-9]|procd: - init -`

// DefaultFailurePattern matches fatal boot failures we should fail fast on.
const DefaultFailurePattern = `Kernel panic|Unable to handle kernel|not syncing|Rebooting in \d+ seconds|Synchronous Exception|CPU \d+ Unable to handle`

// Config parameterizes a smoke run.
type Config struct {
	// ImagePath is the local path to the initramfs-kernel.bin to test.
	ImagePath string
	// ImageName overrides the remote filename (defaults to the basename).
	ImageName string
	// KernelArgs are appended to the bootoctlinux command line.
	KernelArgs string
	// SuccessPattern / FailurePattern are RE2 patterns; empty uses the defaults.
	SuccessPattern string
	FailurePattern string
	// BootTimeout bounds waiting for the success/failure marker after the kernel
	// starts. 0 -> 4m.
	BootTimeout time.Duration
	// NetbootTimeout bounds the U-Boot netboot phase. 0 -> 5m.
	NetbootTimeout time.Duration
	// VerifyCRC32 asks U-Boot to verify the loaded image before booting.
	VerifyCRC32 bool
	// LiveOutput, if set, receives console bytes as they arrive (e.g. os.Stderr).
	LiveOutput io.Writer
	// Progress, if set, receives human-readable progress lines.
	Progress func(string)
}

// Result is the outcome of a smoke run.
type Result struct {
	Success  bool
	Reason   string // why it passed or failed
	BootLog  []byte // full captured console log (the release artifact)
	Uploaded *unwedgev1.UploadImageResponse
	Duration time.Duration
}

func (c *Config) progress(format string, args ...interface{}) {
	if c.Progress != nil {
		c.Progress(fmt.Sprintf(format, args...))
	}
}

// Run performs the smoke test against the daemon reachable via cl.
func Run(ctx context.Context, cl *client.Client, cfg Config) (*Result, error) {
	if cfg.ImagePath == "" {
		return nil, fmt.Errorf("smoke: image path required")
	}
	successRE, err := regexp.Compile(orDefault(cfg.SuccessPattern, DefaultSuccessPattern))
	if err != nil {
		return nil, fmt.Errorf("smoke: bad success pattern: %w", err)
	}
	failureRE, err := regexp.Compile(orDefault(cfg.FailurePattern, DefaultFailurePattern))
	if err != nil {
		return nil, fmt.Errorf("smoke: bad failure pattern: %w", err)
	}
	bootTimeout := cfg.BootTimeout
	if bootTimeout == 0 {
		bootTimeout = 4 * time.Minute
	}
	netbootTimeout := cfg.NetbootTimeout
	if netbootTimeout == 0 {
		netbootTimeout = 5 * time.Minute
	}

	started := time.Now()

	// 1. Upload the freshly built image.
	cfg.progress("uploading image %s", cfg.ImagePath)
	up, err := cl.UploadImageFile(ctx, cfg.ImagePath, true)
	if err != nil {
		return nil, fmt.Errorf("smoke: upload: %w", err)
	}
	cfg.progress("uploaded %s (%d bytes, crc32=%08x)", up.Name, up.Size, up.Crc32)

	// 2. Begin capturing the console BEFORE netbooting, so the log includes the
	//    power-cycle and the entire boot. The capture buffer is the artifact.
	cap := &captureBuf{live: cfg.LiveOutput}
	capCtx, capCancel := context.WithCancel(ctx)
	defer capCancel()
	capDone := make(chan error, 1)
	go func() { capDone <- streamConsole(capCtx, cl, cap) }()
	// Give the stream a moment to establish before power-cycling.
	time.Sleep(300 * time.Millisecond)

	// 3. Netboot: power-cycle, interrupt U-Boot, TFTP, bootoctlinux.
	imageName := cfg.ImageName
	if imageName == "" {
		imageName = up.Name
	}
	cfg.progress("netbooting %s (power-cycle + tftp)", imageName)
	nbCtx, nbCancel := context.WithTimeout(ctx, netbootTimeout)
	defer nbCancel()
	nbErr := cl.Netboot(nbCtx, &unwedgev1.NetbootRequest{
		Image:       imageName,
		PowerCycle:  true,
		KernelArgs:  cfg.KernelArgs,
		VerifyCrc32: cfg.VerifyCRC32,
		TimeoutMs:   netbootTimeout.Milliseconds(),
	}, func(ev *unwedgev1.BootEvent) {
		if ev.Kind == unwedgev1.BootEvent_KIND_STAGE {
			cfg.progress("stage: %s", ev.Stage)
		}
	})
	if nbErr != nil {
		// Netboot failed before the kernel even started; still return the log.
		return finish(cap, started, false, "netboot failed: "+nbErr.Error(), up, capCancel, capDone), nil
	}
	cfg.progress("kernel booting; waiting for healthy userspace")

	// 4. Watch the captured console for a success or failure marker.
	success, reason := waitForMarker(ctx, cap, successRE, failureRE, bootTimeout)

	return finish(cap, started, success, reason, up, capCancel, capDone), nil
}

func finish(cap *captureBuf, started time.Time, ok bool, reason string, up *unwedgev1.UploadImageResponse, cancel context.CancelFunc, done <-chan error) *Result {
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	return &Result{
		Success:  ok,
		Reason:   reason,
		BootLog:  cap.bytes(),
		Uploaded: up,
		Duration: time.Since(started),
	}
}

// waitForMarker polls the growing capture buffer for success/failure patterns.
func waitForMarker(ctx context.Context, cap *captureBuf, successRE, failureRE *regexp.Regexp, timeout time.Duration) (bool, string) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		buf := cap.bytes()
		if loc := failureRE.FindIndex(buf); loc != nil {
			return false, "detected boot failure: " + string(buf[loc[0]:loc[1]])
		}
		if loc := successRE.FindIndex(buf); loc != nil {
			return true, "reached healthy userspace: " + string(buf[loc[0]:loc[1]])
		}
		if time.Now().After(deadline) {
			return false, fmt.Sprintf("timed out after %s waiting for success marker", timeout)
		}
		select {
		case <-ctx.Done():
			return false, "cancelled: " + ctx.Err().Error()
		case <-ticker.C:
		}
	}
}

// streamConsole streams console output into cap until the context is cancelled.
func streamConsole(ctx context.Context, cl *client.Client, cap *captureBuf) error {
	stream, err := cl.API.StreamConsole(ctx, &unwedgev1.StreamConsoleRequest{})
	if err != nil {
		return err
	}
	for {
		chunk, err := stream.Recv()
		if err != nil {
			return err
		}
		cap.write(chunk.GetData())
	}
}

// captureBuf is a synchronized console capture buffer with optional live tee.
type captureBuf struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	live io.Writer
}

func (c *captureBuf) write(p []byte) {
	c.mu.Lock()
	c.buf.Write(p)
	c.mu.Unlock()
	if c.live != nil {
		c.live.Write(p)
	}
}

func (c *captureBuf) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
