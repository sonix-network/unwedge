// Command unwedge is a human/CI client for unwedged. It can drive the
// console, power, U-Boot, image store, and SSH, and run the release smoke test.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	unwedgev1 "github.com/sonix-network/unwedge/gen/unwedge/v1"
	"github.com/sonix-network/unwedge/internal/client"
	"github.com/sonix-network/unwedge/internal/clientconfig"
	"github.com/sonix-network/unwedge/internal/smoke"
)

func main() {
	os.Exit(realMain())
}

const usage = `unwedge - control a vEdge 1000 via unwedged

Usage: unwedge [global flags] <command> [args]

Commands:
  status                       show daemon/target status
  console                      stream the serial console (Ctrl-C to stop)
  log [maxBytes]               print the console scrollback buffer
  write <text>                 send text to the console (use -keys for control keys)
  power <on|off|cycle|status>  control the PDU outlet
  interrupt                    power-cycle and stop at the U-Boot prompt
  uboot <command>              run a single U-Boot command (power-cycles first by default)
  netboot <image>              netboot an image already in the store
  image ls                     list images in the TFTP store
  image upload <file>          upload an image to the store
  image rm <name>              delete an image from the store
  ssh <command>                run a command on the target over SSH
  ssh -W [host:port]           proxy raw SSH to the target (OpenSSH ProxyCommand)
  scp <src> <dst>              copy a file to/from the target (prefix its path ':')
  smoke <image-file>           release smoke test: netboot + verify + boot log

Global flags:`

func realMain() int {
	// Load client defaults (daemon address, TLS material) from the config file
	// and environment so they can be primed once. Precedence: flag > env >
	// config file > built-in default. A -config flag or UNWEDGE_CONFIG picks the
	// file; otherwise ~/.config/unwedge/config.yaml is used if present.
	def, err := clientconfig.Resolve(clientconfig.PreScanConfig(os.Args[1:]))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	fs := flag.NewFlagSet("unwedge", flag.ContinueOnError)
	fs.String("config", clientconfig.DefaultPath(), "client config file (YAML) with addr/ca/cert/key defaults")
	addr := fs.String("addr", def.Addr, "daemon address host:port (port defaults to "+clientconfig.DefaultPort+")")
	ca := fs.String("ca", def.CA, "CA cert file verifying the server")
	cert := fs.String("cert", def.Cert, "client cert file (mTLS)")
	key := fs.String("key", def.Key, "client key file (mTLS)")
	serverName := fs.String("server-name", def.ServerName, "override TLS server name")
	insecureSkip := fs.Bool("insecure", def.Insecure, "skip server certificate verification (dev only)")
	noTLS := fs.Bool("no-tls", def.NoTLS, "connect without TLS (local/testing only)")
	noSRV := fs.Bool("no-srv", def.NoSRV, "disable SRV-record discovery; dial the address and default port directly")
	keys := fs.String("keys", "", "comma-separated control keys for 'write' (e.g. ctrl-x,enter)")
	out := fs.String("out", "", "output file for 'smoke' boot log (default stdout)")
	kernelArgs := fs.String("kernel-args", "", "extra kernel args for netboot/smoke")
	verify := fs.Bool("verify", false, "verify image CRC32 in U-Boot before booting")
	powerCycle := fs.Bool("power-cycle", true, "power-cycle before netboot")
	timeout := fs.Duration("timeout", 8*time.Minute, "overall timeout for the command")
	sessionWait := fs.Duration("session-wait", 10*time.Minute, "how long to wait for the hardware lock if held (0 = wait indefinitely)")
	sessionOwner := fs.String("session-owner", "", "label for who holds the lock (default: user@host)")
	noSession := fs.Bool("no-session", false, "do not acquire the hardware lock (may conflict with other users)")

	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, usage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}
	dialAddr, resolvedName, err := clientconfig.ResolveEndpoint(*addr, !*noSRV, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	// Verify TLS against the queried device name when SRV redirected us to a
	// different controller host; an explicit -server-name still wins.
	sni := *serverName
	if sni == "" {
		sni = resolvedName
	}
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cl, err := client.Dial(client.Options{
		Address: dialAddr, NoTLS: *noTLS, CAFile: *ca, CertFile: *cert, KeyFile: *key,
		ServerName: sni, Insecure: *insecureSkip,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer cl.Close()

	cmd, rest := args[0], args[1:]

	// Re-parse the remaining args with a per-command flag set so operation flags
	// may follow the subcommand (e.g. `unwedge write --keys enter`). Defaults are
	// seeded from the global flags, so placing them before the subcommand still
	// works too. Note: like all Go flag parsing, flags must precede positional
	// arguments (`unwedge netboot --verify <image>`, not `... <image> --verify`).
	sub := flag.NewFlagSet(cmd, flag.ContinueOnError)
	sub.SetOutput(os.Stderr)
	subKeys := sub.String("keys", *keys, "comma-separated control keys for 'write' (e.g. ctrl-x,enter)")
	subOut := sub.String("out", *out, "output file for 'smoke' boot log (default stdout)")
	subKernelArgs := sub.String("kernel-args", *kernelArgs, "extra kernel args for netboot/smoke")
	subVerify := sub.Bool("verify", *verify, "verify image CRC32 in U-Boot before booting")
	subPowerCycle := sub.Bool("power-cycle", *powerCycle, "power-cycle and stop at the U-Boot prompt before netboot/interrupt/uboot")
	subTimeout := sub.Duration("timeout", *timeout, "overall timeout for the command")
	subProxy := sub.Bool("W", false, "'ssh' proxy mode: pipe stdin/stdout to the target's SSH port (ProxyCommand)")
	if err := sub.Parse(rest); err != nil {
		return 2
	}
	rest = sub.Args()
	opts := cmdOpts{
		keys: *subKeys, out: *subOut, kernelArgs: *subKernelArgs,
		verify: *subVerify, powerCycle: *subPowerCycle, timeout: *subTimeout,
		proxy: *subProxy,
	}

	// Acquire the exclusive hardware lock for operational commands. Read-only
	// observation (status, and console/log streaming) is lock-free so watchers
	// don't lock out the driver. Held for the whole command and released on
	// exit; a background ping keeps it alive meanwhile.
	if !*noSession && !lockFreeCommands[cmd] {
		release, err := maybeAcquire(ctx, cl, ownerLabel(*sessionOwner), *sessionWait)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		defer release()
	}

	if err := dispatch(ctx, cl, cmd, rest, opts); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// lockFreeCommands are read-only and do not acquire the hardware lock, matching
// the server-side session-exempt RPCs (GetStatus, StreamConsole, ReadConsoleLog).
var lockFreeCommands = map[string]bool{
	"status":  true,
	"console": true,
	"log":     true,
}

type cmdOpts struct {
	keys       string
	out        string
	kernelArgs string
	verify     bool
	powerCycle bool
	timeout    time.Duration
	proxy      bool
}

func dispatch(ctx context.Context, cl *client.Client, cmd string, args []string, o cmdOpts) error {
	switch cmd {
	case "status":
		return cmdStatus(ctx, cl)
	case "console":
		return cmdConsole(ctx, cl)
	case "log":
		return cmdLog(ctx, cl, args)
	case "write":
		return cmdWrite(ctx, cl, args, o)
	case "power":
		return cmdPower(ctx, cl, args)
	case "interrupt":
		return cmdInterrupt(ctx, cl, o)
	case "uboot":
		return cmdUboot(ctx, cl, args, o)
	case "netboot":
		return cmdNetboot(ctx, cl, args, o)
	case "image":
		return cmdImage(ctx, cl, args)
	case "ssh":
		return cmdSSH(ctx, cl, args, o)
	case "scp":
		return cmdSCP(ctx, cl, args, o)
	case "smoke":
		return cmdSmoke(ctx, cl, args, o)
	default:
		return fmt.Errorf("unknown command %q (see -h)", cmd)
	}
}

func cmdStatus(ctx context.Context, cl *client.Client) error {
	s, err := cl.API.GetStatus(ctx, &unwedgev1.GetStatusRequest{})
	if err != nil {
		return err
	}
	fmt.Printf("version:        %s\n", s.Version)
	fmt.Printf("serial:         %s @ %d (connected=%v)\n", s.SerialDevice, s.SerialBaud, s.SerialConnected)
	powerState := strings.TrimPrefix(s.PowerState.String(), "POWER_STATE_")
	if s.PowerOutlet > 0 {
		fmt.Printf("power state:    %s (outlet %d)\n", powerState, s.PowerOutlet)
	} else {
		fmt.Printf("power state:    %s\n", powerState)
	}
	if s.SshTarget != "" {
		fmt.Printf("ssh target:     %s@%s\n", s.SshUser, s.SshTarget)
	} else {
		fmt.Printf("ssh target:     (not configured)\n")
	}
	fmt.Printf("tftp:           %s (dir %s)\n", s.TftpAddress, s.TftpDir)
	fmt.Printf("console buffer: %d bytes\n", s.ConsoleBufferBytes)
	if s.SessionActive {
		exp := time.UnixMilli(s.SessionExpiresAtUnixMs)
		fmt.Printf("session lock:   HELD by %q (expires in %s)\n", s.SessionOwner, time.Until(exp).Round(time.Second))
	} else {
		fmt.Printf("session lock:   free\n")
	}
	return nil
}

func cmdConsole(ctx context.Context, cl *client.Client) error {
	stream, err := cl.API.StreamConsole(ctx, &unwedgev1.StreamConsoleRequest{ReplayBytes: 8192})
	if err != nil {
		return err
	}
	for {
		chunk, err := stream.Recv()
		if err == io.EOF || ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		os.Stdout.Write(chunk.GetData())
	}
}

func cmdLog(ctx context.Context, cl *client.Client, args []string) error {
	var maxBytes uint32
	if len(args) > 0 {
		fmt.Sscanf(args[0], "%d", &maxBytes)
	}
	resp, err := cl.API.ReadConsoleLog(ctx, &unwedgev1.ReadConsoleLogRequest{MaxBytes: maxBytes})
	if err != nil {
		return err
	}
	os.Stdout.Write(resp.GetData())
	if resp.GetTruncated() {
		fmt.Fprintln(os.Stderr, "\n[buffer truncated]")
	}
	return nil
}

func cmdWrite(ctx context.Context, cl *client.Client, args []string, o cmdOpts) error {
	req := &unwedgev1.WriteConsoleRequest{Data: []byte(strings.Join(args, " "))}
	if o.keys != "" {
		req.Keys = strings.Split(o.keys, ",")
	}
	resp, err := cl.API.WriteConsole(ctx, req)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %d bytes\n", resp.GetBytesWritten())
	return nil
}

func cmdPower(ctx context.Context, cl *client.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("power requires on|off|cycle|status")
	}
	action := map[string]unwedgev1.PowerAction{
		"on":     unwedgev1.PowerAction_POWER_ACTION_ON,
		"off":    unwedgev1.PowerAction_POWER_ACTION_OFF,
		"cycle":  unwedgev1.PowerAction_POWER_ACTION_CYCLE,
		"status": unwedgev1.PowerAction_POWER_ACTION_STATUS,
	}[args[0]]
	if action == unwedgev1.PowerAction_POWER_ACTION_UNSPECIFIED {
		return fmt.Errorf("invalid power action %q", args[0])
	}
	resp, err := cl.API.PowerControl(ctx, &unwedgev1.PowerControlRequest{Action: action})
	if err != nil {
		return err
	}
	fmt.Printf("power state: %s\n", strings.TrimPrefix(resp.State.String(), "POWER_STATE_"))
	if resp.Detail != "" {
		fmt.Println(resp.Detail)
	}
	return nil
}

func cmdInterrupt(ctx context.Context, cl *client.Client, o cmdOpts) error {
	cctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	return cl.InterruptBoot(cctx, &unwedgev1.InterruptBootRequest{
		PowerCycle: o.powerCycle, TimeoutMs: o.timeout.Milliseconds(),
	}, printBootEvent)
}

func cmdUboot(ctx context.Context, cl *client.Client, args []string, o cmdOpts) error {
	if len(args) == 0 {
		return fmt.Errorf("uboot requires a command")
	}
	cctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()

	// With power-cycle (the default), first reuse the exact interrupt path
	// (power-cycle, catch the autoboot prompt, stop at the U-Boot prompt) before
	// running the command. Firing the command blindly misses the vEdge's very
	// short autoboot window and lets the board boot into the OS (issue #9).
	if o.powerCycle {
		// Route the interrupt phase's boot console to stderr so stdout carries
		// only the command's own output (e.g. `unwedge uboot printenv > env`).
		if err := cl.InterruptBoot(cctx, &unwedgev1.InterruptBootRequest{
			PowerCycle: true, TimeoutMs: o.timeout.Milliseconds(),
		}, bootEventTo(os.Stderr)); err != nil {
			return err
		}
	}

	resp, err := cl.API.RunUbootCommand(cctx, &unwedgev1.RunUbootCommandRequest{
		Command: strings.Join(args, " "), TimeoutMs: o.timeout.Milliseconds(),
	})
	if err != nil {
		return err
	}
	fmt.Print(resp.GetOutput())
	if !resp.GetPromptReturned() {
		fmt.Fprintln(os.Stderr, "[warning: U-Boot prompt did not return]")
	}
	return nil
}

func cmdNetboot(ctx context.Context, cl *client.Client, args []string, o cmdOpts) error {
	if len(args) == 0 {
		return fmt.Errorf("netboot requires an image name")
	}
	cctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	return cl.Netboot(cctx, &unwedgev1.NetbootRequest{
		Image: args[0], PowerCycle: o.powerCycle, KernelArgs: o.kernelArgs,
		VerifyCrc32: o.verify, TimeoutMs: o.timeout.Milliseconds(),
	}, printBootEvent)
}

func cmdImage(ctx context.Context, cl *client.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("image requires ls|upload|rm")
	}
	switch args[0] {
	case "ls":
		resp, err := cl.API.ListImages(ctx, &unwedgev1.ListImagesRequest{})
		if err != nil {
			return err
		}
		if len(resp.Images) == 0 {
			fmt.Println("(no images)")
		}
		for _, im := range resp.Images {
			fmt.Printf("%-60s %10d  crc32=%08x  %s\n", im.Name, im.Size, im.Crc32,
				time.Unix(im.ModTimeUnix, 0).Format(time.RFC3339))
		}
		return nil
	case "upload":
		if len(args) < 2 {
			return fmt.Errorf("image upload requires a file path")
		}
		resp, err := cl.UploadImageFile(ctx, args[1], true)
		if err != nil {
			return err
		}
		fmt.Printf("uploaded %s: %d bytes, crc32=%08x, sha256=%s\n", resp.Name, resp.Size, resp.Crc32, resp.Sha256)
		return nil
	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("image rm requires a name")
		}
		_, err := cl.API.DeleteImage(ctx, &unwedgev1.DeleteImageRequest{Name: args[1]})
		if err == nil {
			fmt.Printf("deleted %s\n", args[1])
		}
		return err
	default:
		return fmt.Errorf("unknown image subcommand %q", args[0])
	}
}

func cmdSSH(ctx context.Context, cl *client.Client, args []string, o cmdOpts) error {
	// Proxy mode (-W): tunnel raw SSH bytes so a local ssh/scp can reach the
	// target through the daemon, e.g. ssh -o ProxyCommand="unwedge ssh -W".
	// No command timeout applies; the session runs until either end closes. Any
	// host:port args (e.g. OpenSSH's %h:%p) are ignored: the daemon always dials
	// its server-configured SSH target.
	if o.proxy {
		return cl.Tunnel(ctx, os.Stdin, os.Stdout)
	}
	if len(args) == 0 {
		return fmt.Errorf("ssh requires a command (or -W for proxy mode)")
	}
	resp, err := cl.API.SSHExec(ctx, &unwedgev1.SSHExecRequest{
		Command: strings.Join(args, " "), TimeoutMs: o.timeout.Milliseconds(),
	})
	if err != nil {
		return err
	}
	os.Stdout.Write(resp.GetStdout())
	os.Stderr.Write(resp.GetStderr())
	if resp.GetTimedOut() {
		return fmt.Errorf("ssh command timed out")
	}
	if resp.GetExitCode() != 0 {
		return fmt.Errorf("ssh command exited %d", resp.GetExitCode())
	}
	return nil
}

// cmdSCP copies a file to or from the target. Exactly one of <src>/<dst> must be
// a target-side path, marked by a leading ':' (e.g. "unwedge scp ./f :/tmp/f"
// uploads; "unwedge scp :/tmp/f ./f" downloads).
func cmdSCP(ctx context.Context, cl *client.Client, args []string, o cmdOpts) error {
	if len(args) != 2 {
		return fmt.Errorf("scp requires <src> <dst>; prefix the target-side path with ':'")
	}
	src, dst := args[0], args[1]
	srcRemote, dstRemote := strings.HasPrefix(src, ":"), strings.HasPrefix(dst, ":")
	switch {
	case srcRemote && !dstRemote:
		n, err := cl.SCPDownloadFile(ctx, strings.TrimPrefix(src, ":"), dst, o.timeout)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "downloaded %d bytes to %s\n", n, dst)
		return nil
	case dstRemote && !srcRemote:
		n, err := cl.SCPUploadFile(ctx, src, strings.TrimPrefix(dst, ":"), o.timeout)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "uploaded %d bytes to %s\n", n, dst)
		return nil
	default:
		return fmt.Errorf("exactly one of <src>/<dst> must be a target path prefixed with ':'")
	}
}

func cmdSmoke(ctx context.Context, cl *client.Client, args []string, o cmdOpts) error {
	if len(args) == 0 {
		return fmt.Errorf("smoke requires an image file path")
	}
	res, err := smoke.Run(ctx, cl, smoke.Config{
		ImagePath:   args[0],
		KernelArgs:  o.kernelArgs,
		VerifyCRC32: o.verify,
		BootTimeout: o.timeout,
		LiveOutput:  os.Stderr,
		Progress:    func(s string) { fmt.Fprintln(os.Stderr, "smoke:", s) },
	})
	if err != nil {
		return err
	}
	// Write the boot log artifact.
	if o.out != "" {
		if werr := os.WriteFile(o.out, res.BootLog, 0o644); werr != nil {
			return fmt.Errorf("write boot log: %w", werr)
		}
		fmt.Fprintf(os.Stderr, "smoke: boot log written to %s (%d bytes)\n", o.out, len(res.BootLog))
	}
	fmt.Fprintf(os.Stderr, "smoke: %s in %s: %s\n", passFail(res.Success), res.Duration.Round(time.Second), res.Reason)
	if !res.Success {
		return fmt.Errorf("smoke test FAILED: %s", res.Reason)
	}
	return nil
}

// printBootEvent streams boot console output to stdout and progress to stderr.
func printBootEvent(ev *unwedgev1.BootEvent) { bootEventTo(os.Stdout)(ev) }

// bootEventTo returns a BootEventHandler that writes console output to consoleW
// and progress lines (stage/error/success/info) to stderr. Callers that need
// stdout kept clean for a command's own output pass os.Stderr as consoleW.
func bootEventTo(consoleW io.Writer) client.BootEventHandler {
	return func(ev *unwedgev1.BootEvent) {
		switch ev.Kind {
		case unwedgev1.BootEvent_KIND_CONSOLE:
			consoleW.Write(ev.GetConsole())
		case unwedgev1.BootEvent_KIND_STAGE:
			fmt.Fprintf(os.Stderr, "== %s: %s\n", ev.Stage, ev.Message)
		case unwedgev1.BootEvent_KIND_ERROR:
			fmt.Fprintf(os.Stderr, "!! error: %s\n", ev.Message)
		case unwedgev1.BootEvent_KIND_SUCCESS:
			fmt.Fprintf(os.Stderr, "** %s\n", ev.Message)
		default:
			if ev.Message != "" {
				fmt.Fprintf(os.Stderr, "-- %s\n", ev.Message)
			}
		}
	}
}

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

// ownerLabel returns the session owner label, defaulting to user@host.
func ownerLabel(override string) string {
	if override != "" {
		return override
	}
	u := os.Getenv("USER")
	if u == "" {
		u = "unwedge"
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return u + "@" + h
	}
	return u
}

// maybeAcquire grabs the hardware lock, waiting if it is held. It returns a
// release func (always non-nil). If the daemon has session locking disabled it
// silently proceeds without a lock.
func maybeAcquire(ctx context.Context, cl *client.Client, owner string, wait time.Duration) (func(), error) {
	noop := func() {}
	// Fast, non-blocking attempt first so we can print a friendly wait notice.
	sess, err := cl.Acquire(ctx, owner, -1)
	if err != nil {
		switch status.Code(err) {
		case codes.Unimplemented:
			return noop, nil // session locking disabled on the daemon
		case codes.FailedPrecondition: // currently held by someone else
			fmt.Fprintf(os.Stderr, "unwedge: %s; waiting for the hardware lock...\n", status.Convert(err).Message())
			sess, err = cl.Acquire(ctx, owner, wait)
			if err != nil {
				return noop, err
			}
		default:
			return noop, err
		}
	}
	stop := cl.StartKeepalive(sess.TTL / 3)
	return func() {
		stop()
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = cl.Release(rctx)
	}, nil
}
