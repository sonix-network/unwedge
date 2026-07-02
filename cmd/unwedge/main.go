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

	unwedgev1 "github.com/sonix-network/unwedge/gen/unwedge/v1"
	"github.com/sonix-network/unwedge/internal/client"
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
  uboot <command>              run a single U-Boot command
  netboot <image>              netboot an image already in the store
  image ls                     list images in the TFTP store
  image upload <file>          upload an image to the store
  image rm <name>              delete an image from the store
  ssh <command>                run a command on the target over SSH
  smoke <image-file>           release smoke test: netboot + verify + boot log

Global flags:`

func realMain() int {
	fs := flag.NewFlagSet("unwedge", flag.ContinueOnError)
	addr := fs.String("addr", envOr("UNWEDGE_ADDR", "localhost:7777"), "daemon address host:port")
	ca := fs.String("ca", os.Getenv("UNWEDGE_CA"), "CA cert file verifying the server")
	cert := fs.String("cert", os.Getenv("UNWEDGE_CERT"), "client cert file (mTLS)")
	key := fs.String("key", os.Getenv("UNWEDGE_KEY"), "client key file (mTLS)")
	serverName := fs.String("server-name", os.Getenv("UNWEDGE_SERVER_NAME"), "override TLS server name")
	insecureSkip := fs.Bool("insecure", false, "skip server certificate verification (dev only)")
	noTLS := fs.Bool("no-tls", false, "connect without TLS (local/testing only)")
	keys := fs.String("keys", "", "comma-separated control keys for 'write' (e.g. ctrl-x,enter)")
	out := fs.String("out", "", "output file for 'smoke' boot log (default stdout)")
	kernelArgs := fs.String("kernel-args", "", "extra kernel args for netboot/smoke")
	verify := fs.Bool("verify", false, "verify image CRC32 in U-Boot before booting")
	powerCycle := fs.Bool("power-cycle", true, "power-cycle before netboot")
	timeout := fs.Duration("timeout", 8*time.Minute, "overall timeout for the command")

	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, usage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cl, err := client.Dial(client.Options{
		Address: *addr, NoTLS: *noTLS, CAFile: *ca, CertFile: *cert, KeyFile: *key,
		ServerName: *serverName, Insecure: *insecureSkip,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer cl.Close()

	cmd, rest := args[0], args[1:]
	opts := cmdOpts{
		keys: *keys, out: *out, kernelArgs: *kernelArgs,
		verify: *verify, powerCycle: *powerCycle, timeout: *timeout,
	}
	if err := dispatch(ctx, cl, cmd, rest, opts); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

type cmdOpts struct {
	keys       string
	out        string
	kernelArgs string
	verify     bool
	powerCycle bool
	timeout    time.Duration
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
	fmt.Printf("power state:    %s\n", strings.TrimPrefix(s.PowerState.String(), "POWER_STATE_"))
	fmt.Printf("tftp:           %s (dir %s)\n", s.TftpAddress, s.TftpDir)
	fmt.Printf("console buffer: %d bytes\n", s.ConsoleBufferBytes)
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
	resp, err := cl.API.RunUbootCommand(ctx, &unwedgev1.RunUbootCommandRequest{
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
	if len(args) == 0 {
		return fmt.Errorf("ssh requires a command")
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

func printBootEvent(ev *unwedgev1.BootEvent) {
	switch ev.Kind {
	case unwedgev1.BootEvent_KIND_CONSOLE:
		os.Stdout.Write(ev.GetConsole())
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

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
