// Command unwedge-mcp is a Model Context Protocol server that an AI agent runs
// locally. It bridges MCP tool calls to a remote unwedged daemon over gRPC/TLS,
// exposing the vEdge 1000's console, power, U-Boot, images, SSH, file transfer
// (scp), and the release smoke test as tools.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
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
	"github.com/sonix-network/unwedge/internal/mcp"
	"github.com/sonix-network/unwedge/internal/smoke"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "unwedge-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	// Client defaults (daemon address, TLS material) load from the config file
	// and environment; precedence is flag > env > config file > built-in. A
	// -config flag or UNWEDGE_CONFIG picks the file, else ~/.config/unwedge/config.yaml.
	def, err := clientconfig.Resolve(clientconfig.PreScanConfig(os.Args[1:]))
	if err != nil {
		return err
	}

	flag.String("config", clientconfig.DefaultPath(), "client config file (YAML) with addr/ca/cert/key defaults")
	addr := flag.String("addr", def.Addr, "daemon address host:port (port defaults to "+clientconfig.DefaultPort+")")
	ca := flag.String("ca", def.CA, "CA cert verifying the server")
	cert := flag.String("cert", def.Cert, "client cert (mTLS)")
	key := flag.String("key", def.Key, "client key (mTLS)")
	serverName := flag.String("server-name", def.ServerName, "override TLS server name")
	insecure := flag.Bool("insecure", def.Insecure, "skip server cert verification (dev only)")
	noTLS := flag.Bool("no-tls", def.NoTLS, "connect without TLS (local/testing only)")
	noSRV := flag.Bool("no-srv", def.NoSRV, "disable SRV-record discovery; dial the address and default port directly")
	sessionOwner := flag.String("session-owner", "", "hardware-lock owner label (default: unwedge-mcp@host)")
	sessionWait := flag.Duration("session-wait", 10*time.Minute, "how long a tool call waits for the hardware lock if held")
	flag.Parse()
	dialAddr, resolvedName, err := clientconfig.ResolveEndpoint(*addr, !*noSRV, nil)
	if err != nil {
		return err
	}
	sni := *serverName
	if sni == "" {
		sni = resolvedName
	}

	cl, err := client.Dial(client.Options{
		Address: dialAddr, NoTLS: *noTLS, CAFile: *ca, CertFile: *cert, KeyFile: *key,
		ServerName: sni, Insecure: *insecure,
	})
	if err != nil {
		return err
	}
	defer cl.Close()

	owner := ownerLabel(*sessionOwner)
	srv := mcp.NewServer("unwedge", version, os.Stdin, os.Stdout)
	registerTools(srv, cl, owner, *sessionWait)

	// Guard operational tools with the hardware lock: lazily acquire on first
	// use, auto-refresh via each call (server-side), and re-acquire if the lock
	// was lost (e.g. it expired after >TTL idle). Introspection and the explicit
	// lock tools are exempt. No background keepalive, so an idle agent releases.
	// Read-only tools observe without the lock (matching the server's exempt
	// RPCs), so watchers don't lock out the driver.
	lockExempt := map[string]bool{
		"get_status": true, "read_console_log": true,
		"acquire_lock": true, "release_lock": true,
	}
	srv.Use(func(name string, next mcp.ToolHandler) mcp.ToolHandler {
		if lockExempt[name] {
			return next
		}
		return func(ctx context.Context, args json.RawMessage) (string, error) {
			return callWithSession(ctx, cl, owner, *sessionWait, func() (string, error) { return next(ctx, args) })
		}
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Release the lock when the bridge shuts down.
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = cl.Release(rctx)
	}()
	fmt.Fprintf(os.Stderr, "unwedge-mcp: connected to %s, serving MCP on stdio\n", *addr)
	return srv.Serve(ctx)
}

// obj is a helper for building JSON schema fragments.
type obj = map[string]interface{}

func schema(props obj, required ...string) obj {
	s := obj{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func registerTools(srv *mcp.Server, cl *client.Client, owner string, wait time.Duration) {
	srv.AddTool(mcp.Tool{
		Name:        "get_status",
		Description: "Get controller/target status: serial connection, power state and outlet, SSH target, TFTP dir, and hardware-lock (session) state. Does not acquire the lock.",
		InputSchema: schema(obj{}),
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			s, err := cl.API.GetStatus(ctx, &unwedgev1.GetStatusRequest{})
			if err != nil {
				return "", err
			}
			lock := "free"
			if s.SessionActive {
				lock = fmt.Sprintf("HELD by %q (expires in %s)", s.SessionOwner,
					time.Until(time.UnixMilli(s.SessionExpiresAtUnixMs)).Round(time.Second))
			}
			power := strings.TrimPrefix(s.PowerState.String(), "POWER_STATE_")
			if s.PowerOutlet > 0 {
				power = fmt.Sprintf("%s(outlet%d)", power, s.PowerOutlet)
			}
			ssh := "(unconfigured)"
			if s.SshTarget != "" {
				ssh = fmt.Sprintf("%s@%s", s.SshUser, s.SshTarget)
			}
			return fmt.Sprintf("version=%s serial=%s@%d connected=%v power=%s ssh=%s tftp=%s dir=%s buffer=%dB lock=%s",
				s.Version, s.SerialDevice, s.SerialBaud, s.SerialConnected,
				power, ssh, s.TftpAddress, s.TftpDir, s.ConsoleBufferBytes, lock), nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name:        "acquire_lock",
		Description: "Explicitly acquire the exclusive hardware lock and hold it (operational tools auto-acquire, but use this to grab the unit up front for a long sequence). Blocks if another client holds it.",
		InputSchema: schema(obj{}),
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			if cl.HasSession() {
				return "already holding the hardware lock", nil
			}
			sess, err := cl.Acquire(ctx, owner, wait)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("acquired hardware lock (expires in %s)", time.Until(sess.ExpiresAt).Round(time.Second)), nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name:        "release_lock",
		Description: "Release the exclusive hardware lock so another client can use the unit. The lock also auto-releases after the idle TTL.",
		InputSchema: schema(obj{}),
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			if !cl.HasSession() {
				return "no hardware lock held", nil
			}
			if err := cl.Release(ctx); err != nil {
				return "", err
			}
			return "released hardware lock", nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name:        "read_console_log",
		Description: "Read the recent serial console scrollback buffer. Optionally limit to the last max_bytes bytes.",
		InputSchema: schema(obj{"max_bytes": obj{"type": "integer", "description": "return only the last N bytes"}}),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				MaxBytes uint32 `json:"max_bytes"`
			}
			_ = json.Unmarshal(args, &a)
			r, err := cl.API.ReadConsoleLog(ctx, &unwedgev1.ReadConsoleLogRequest{MaxBytes: a.MaxBytes})
			if err != nil {
				return "", err
			}
			return string(r.Data), nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name: "write_console",
		Description: "Send text and/or named control keys to the serial console. " +
			"Keys include: enter, esc, space, ctrl-x (interrupt U-Boot), ctrl-c, ctrl-a..ctrl-z.",
		InputSchema: schema(obj{
			"text": obj{"type": "string", "description": "raw text to send"},
			"keys": obj{"type": "array", "items": obj{"type": "string"}, "description": "control keys to append"},
		}),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Text string   `json:"text"`
				Keys []string `json:"keys"`
			}
			_ = json.Unmarshal(args, &a)
			r, err := cl.API.WriteConsole(ctx, &unwedgev1.WriteConsoleRequest{Data: []byte(a.Text), Keys: a.Keys})
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes", r.BytesWritten), nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name: "wait_for_pattern",
		Description: "Block until a regular expression (RE2) matches new console output, or the timeout elapses. " +
			"Arm this before triggering the action that produces the expected output.",
		InputSchema: schema(obj{
			"pattern":                  obj{"type": "string"},
			"timeout_ms":               obj{"type": "integer"},
			"include_scrollback_bytes": obj{"type": "integer"},
		}, "pattern"),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Pattern                string `json:"pattern"`
				TimeoutMs              int64  `json:"timeout_ms"`
				IncludeScrollbackBytes uint32 `json:"include_scrollback_bytes"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			r, err := cl.API.WaitForPattern(ctx, &unwedgev1.WaitForPatternRequest{
				Pattern: a.Pattern, TimeoutMs: a.TimeoutMs, IncludeScrollbackBytes: a.IncludeScrollbackBytes,
			})
			if err != nil {
				return "", err
			}
			if !r.Matched {
				return fmt.Sprintf("no match after %dms", r.ElapsedMs), nil
			}
			return fmt.Sprintf("matched %q after %dms", r.Match, r.ElapsedMs), nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name:        "power",
		Description: "Control the target's PDU outlet. action is one of: on, off, cycle, status.",
		InputSchema: schema(obj{"action": obj{"type": "string", "enum": []string{"on", "off", "cycle", "status"}}}, "action"),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Action string `json:"action"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			action, ok := map[string]unwedgev1.PowerAction{
				"on":     unwedgev1.PowerAction_POWER_ACTION_ON,
				"off":    unwedgev1.PowerAction_POWER_ACTION_OFF,
				"cycle":  unwedgev1.PowerAction_POWER_ACTION_CYCLE,
				"status": unwedgev1.PowerAction_POWER_ACTION_STATUS,
			}[a.Action]
			if !ok {
				return "", fmt.Errorf("invalid action %q", a.Action)
			}
			r, err := cl.API.PowerControl(ctx, &unwedgev1.PowerControlRequest{Action: action})
			if err != nil {
				return "", err
			}
			return "power state: " + strings.TrimPrefix(r.State.String(), "POWER_STATE_"), nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name:        "run_uboot_command",
		Description: "Run a single command at the U-Boot prompt and return its output. Assumes U-Boot is at its prompt (see interrupt_boot).",
		InputSchema: schema(obj{
			"command":    obj{"type": "string"},
			"timeout_ms": obj{"type": "integer"},
		}, "command"),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Command   string `json:"command"`
				TimeoutMs int64  `json:"timeout_ms"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			r, err := cl.API.RunUbootCommand(ctx, &unwedgev1.RunUbootCommandRequest{Command: a.Command, TimeoutMs: a.TimeoutMs})
			if err != nil {
				return "", err
			}
			out := r.Output
			if !r.PromptReturned {
				out += "\n[warning: prompt did not return]"
			}
			return out, nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name:        "interrupt_boot",
		Description: "Power-cycle the target and stop at the U-Boot prompt. Returns the captured boot events.",
		InputSchema: schema(obj{"power_cycle": obj{"type": "boolean", "description": "power-cycle first (default true)"}}),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			a := struct {
				PowerCycle *bool `json:"power_cycle"`
			}{}
			_ = json.Unmarshal(args, &a)
			pc := a.PowerCycle == nil || *a.PowerCycle
			cctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
			defer cancel()
			var sb strings.Builder
			err := cl.InterruptBoot(cctx, &unwedgev1.InterruptBootRequest{PowerCycle: pc, TimeoutMs: (4 * time.Minute).Milliseconds()},
				bootEventCollector(&sb))
			if err != nil {
				return sb.String(), err
			}
			return sb.String() + "\nreached U-Boot prompt", nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name:        "netboot",
		Description: "Netboot an image already present in the TFTP store: power-cycle, interrupt U-Boot, DHCP, TFTP, bootoctlinux. Returns boot events.",
		InputSchema: schema(obj{
			"image":        obj{"type": "string"},
			"power_cycle":  obj{"type": "boolean", "description": "default true"},
			"kernel_args":  obj{"type": "string"},
			"verify_crc32": obj{"type": "boolean"},
			"timeout_ms":   obj{"type": "integer"},
		}, "image"),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			a := struct {
				Image       string `json:"image"`
				PowerCycle  *bool  `json:"power_cycle"`
				KernelArgs  string `json:"kernel_args"`
				VerifyCRC32 bool   `json:"verify_crc32"`
				TimeoutMs   int64  `json:"timeout_ms"`
			}{}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			pc := a.PowerCycle == nil || *a.PowerCycle
			timeout := 5 * time.Minute
			if a.TimeoutMs > 0 {
				timeout = time.Duration(a.TimeoutMs) * time.Millisecond
			}
			cctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
			defer cancel()
			var sb strings.Builder
			err := cl.Netboot(cctx, &unwedgev1.NetbootRequest{
				Image: a.Image, PowerCycle: pc, KernelArgs: a.KernelArgs,
				VerifyCrc32: a.VerifyCRC32, TimeoutMs: timeout.Milliseconds(),
			}, bootEventCollector(&sb))
			if err != nil {
				return sb.String(), err
			}
			return sb.String(), nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name:        "list_images",
		Description: "List images available in the TFTP store, with sizes and CRC32s.",
		InputSchema: schema(obj{}),
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			r, err := cl.API.ListImages(ctx, &unwedgev1.ListImagesRequest{})
			if err != nil {
				return "", err
			}
			if len(r.Images) == 0 {
				return "(no images)", nil
			}
			var sb strings.Builder
			for _, im := range r.Images {
				fmt.Fprintf(&sb, "%s\t%d bytes\tcrc32=%08x\n", im.Name, im.Size, im.Crc32)
			}
			return sb.String(), nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name:        "upload_image",
		Description: "Upload a local image file (path on the machine running this MCP server) into the target's TFTP store.",
		InputSchema: schema(obj{
			"path":      obj{"type": "string"},
			"overwrite": obj{"type": "boolean"},
		}, "path"),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path      string `json:"path"`
				Overwrite bool   `json:"overwrite"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			r, err := cl.UploadImageFile(ctx, a.Path, a.Overwrite)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("uploaded %s: %d bytes, crc32=%08x, sha256=%s", r.Name, r.Size, r.Crc32, r.Sha256), nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name:        "delete_image",
		Description: "Delete an image from the target's TFTP store.",
		InputSchema: schema(obj{"name": obj{"type": "string"}}, "name"),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			if _, err := cl.API.DeleteImage(ctx, &unwedgev1.DeleteImageRequest{Name: a.Name}); err != nil {
				return "", err
			}
			return "deleted " + a.Name, nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name:        "ssh_exec",
		Description: "Run a shell command on the booted target over SSH and return stdout/stderr and exit code.",
		InputSchema: schema(obj{
			"command":    obj{"type": "string"},
			"timeout_ms": obj{"type": "integer"},
		}, "command"),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Command   string `json:"command"`
				TimeoutMs int64  `json:"timeout_ms"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			r, err := cl.API.SSHExec(ctx, &unwedgev1.SSHExecRequest{Command: a.Command, TimeoutMs: a.TimeoutMs})
			if err != nil {
				return "", err
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "exit=%d timed_out=%v\n", r.ExitCode, r.TimedOut)
			if len(r.Stdout) > 0 {
				fmt.Fprintf(&sb, "--- stdout ---\n%s\n", r.Stdout)
			}
			if len(r.Stderr) > 0 {
				fmt.Fprintf(&sb, "--- stderr ---\n%s\n", r.Stderr)
			}
			return sb.String(), nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name: "scp_upload",
		Description: "Copy a local file (path on the machine running this MCP server) to the booted target over SSH " +
			"(classic scp protocol). Use for pushing test binaries, configs, or scripts onto the DUT.",
		InputSchema: schema(obj{
			"local_path":  obj{"type": "string", "description": "source path on the MCP host"},
			"remote_path": obj{"type": "string", "description": "destination path on the target"},
			"timeout_ms":  obj{"type": "integer"},
		}, "local_path", "remote_path"),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				LocalPath  string `json:"local_path"`
				RemotePath string `json:"remote_path"`
				TimeoutMs  int64  `json:"timeout_ms"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			n, err := cl.SCPUploadFile(ctx, a.LocalPath, a.RemotePath, scpTimeout(a.TimeoutMs))
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("uploaded %d bytes to %s", n, a.RemotePath), nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name: "scp_download",
		Description: "Copy a file from the booted target to the machine running this MCP server over SSH " +
			"(classic scp protocol). Use for pulling logs, cores, or captured output off the DUT.",
		InputSchema: schema(obj{
			"remote_path": obj{"type": "string", "description": "source path on the target"},
			"local_path":  obj{"type": "string", "description": "destination path on the MCP host"},
			"timeout_ms":  obj{"type": "integer"},
		}, "remote_path", "local_path"),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				RemotePath string `json:"remote_path"`
				LocalPath  string `json:"local_path"`
				TimeoutMs  int64  `json:"timeout_ms"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			n, err := cl.SCPDownloadFile(ctx, a.RemotePath, a.LocalPath, scpTimeout(a.TimeoutMs))
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("downloaded %d bytes to %s", n, a.LocalPath), nil
		},
	})

	srv.AddTool(mcp.Tool{
		Name: "smoke_test",
		Description: "Release smoke test: upload a local initramfs image, netboot it, and verify the board reaches healthy " +
			"userspace. Returns PASS/FAIL and the boot log; optionally writes the boot log to out_path.",
		InputSchema: schema(obj{
			"image_path":      obj{"type": "string"},
			"out_path":        obj{"type": "string", "description": "write the boot log here (on the MCP host)"},
			"kernel_args":     obj{"type": "string"},
			"verify_crc32":    obj{"type": "boolean"},
			"boot_timeout_ms": obj{"type": "integer"},
		}, "image_path"),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				ImagePath     string `json:"image_path"`
				OutPath       string `json:"out_path"`
				KernelArgs    string `json:"kernel_args"`
				VerifyCRC32   bool   `json:"verify_crc32"`
				BootTimeoutMs int64  `json:"boot_timeout_ms"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			cfg := smoke.Config{
				ImagePath:   a.ImagePath,
				KernelArgs:  a.KernelArgs,
				VerifyCRC32: a.VerifyCRC32,
			}
			if a.BootTimeoutMs > 0 {
				cfg.BootTimeout = time.Duration(a.BootTimeoutMs) * time.Millisecond
			}
			res, err := smoke.Run(ctx, cl, cfg)
			if err != nil {
				return "", err
			}
			if a.OutPath != "" {
				if werr := os.WriteFile(a.OutPath, res.BootLog, 0o644); werr != nil {
					return "", werr
				}
			}
			status := "PASS"
			if !res.Success {
				status = "FAIL"
			}
			return fmt.Sprintf("%s in %s: %s\n(boot log: %d bytes%s)",
				status, res.Duration.Round(time.Second), res.Reason, len(res.BootLog),
				outNote(a.OutPath)), nil
		},
	})
}

func bootEventCollector(sb *strings.Builder) client.BootEventHandler {
	return func(ev *unwedgev1.BootEvent) {
		switch ev.Kind {
		case unwedgev1.BootEvent_KIND_CONSOLE:
			sb.Write(ev.Console)
		case unwedgev1.BootEvent_KIND_STAGE:
			fmt.Fprintf(sb, "\n== %s: %s\n", ev.Stage, ev.Message)
		case unwedgev1.BootEvent_KIND_ERROR:
			fmt.Fprintf(sb, "\n!! %s\n", ev.Message)
		case unwedgev1.BootEvent_KIND_INFO:
			fmt.Fprintf(sb, "\n-- %s\n", ev.Message)
		}
		// Keep the summary bounded.
		if sb.Len() > 200_000 {
			s := sb.String()
			sb.Reset()
			sb.WriteString(s[len(s)-100_000:])
		}
	}
}

// scpTimeout defaults an unset/zero timeout to 5 minutes.
func scpTimeout(ms int64) time.Duration {
	if ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return 5 * time.Minute
}

func outNote(p string) string {
	if p == "" {
		return ""
	}
	return ", written to " + p
}

// ownerLabel defaults the hardware-lock owner to unwedge-mcp@host.
func ownerLabel(override string) string {
	if override != "" {
		return override
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return "unwedge-mcp@" + h
	}
	return "unwedge-mcp"
}

// callWithSession ensures the hardware lock is held, runs fn, and if the daemon
// reports the session was lost (expired after idle), re-acquires and retries once.
func callWithSession(ctx context.Context, cl *client.Client, owner string, wait time.Duration, fn func() (string, error)) (string, error) {
	if err := ensureSession(ctx, cl, owner, wait); err != nil {
		return "", err
	}
	text, err := fn()
	if err != nil && sessionLost(err) {
		cl.ClearSession()
		if err2 := ensureSession(ctx, cl, owner, wait); err2 != nil {
			return "", err2
		}
		return fn()
	}
	return text, err
}

func ensureSession(ctx context.Context, cl *client.Client, owner string, wait time.Duration) error {
	if cl.HasSession() {
		return nil
	}
	_, err := cl.Acquire(ctx, owner, wait)
	if err != nil && status.Code(err) == codes.Unimplemented {
		return nil // session locking disabled on the daemon
	}
	return err
}

func sessionLost(err error) bool {
	return status.Code(err) == codes.FailedPrecondition &&
		strings.Contains(strings.ToLower(status.Convert(err).Message()), "session")
}
