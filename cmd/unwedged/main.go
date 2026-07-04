// Command unwedged is the controller-side daemon. It owns the vEdge 1000's
// serial console, power (via APC PDU/SNMP), a TFTP server for netbooting, and
// SSH to the target, exposing them as a gRPC API (TLS by default) for an AI
// agent to drive kernel/OS development.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	unwedgev1 "github.com/sonix-network/unwedge/gen/unwedge/v1"
	"github.com/sonix-network/unwedge/internal/config"
	"github.com/sonix-network/unwedge/internal/power"
	"github.com/sonix-network/unwedge/internal/serialconsole"
	"github.com/sonix-network/unwedge/internal/serialport"
	"github.com/sonix-network/unwedge/internal/server"
	"github.com/sonix-network/unwedge/internal/session"
	"github.com/sonix-network/unwedge/internal/sshexec"
	"github.com/sonix-network/unwedge/internal/tftp"
	"github.com/sonix-network/unwedge/internal/tlsutil"
	"github.com/sonix-network/unwedge/internal/uboot"
)

// version is overridable via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "unwedged:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/unwedge/config.yaml", "path to YAML config")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// Tag every log line with the instance name so multiple instances sharing
	// one controller's log stream stay attributable. Omitted when unnamed
	// (single-instance) to keep the legacy log format unchanged.
	if cfg.Name != "" {
		logger = logger.With("instance", cfg.Name)
		slog.SetDefault(logger)
	}

	// Serial console (required).
	port, err := serialport.Open(cfg.Serial.Device, cfg.Serial.Baud)
	if err != nil {
		return fmt.Errorf("open serial: %w", err)
	}
	console := serialconsole.New(port, cfg.Serial.BufferBytes)
	defer console.Close()
	logger.Info("serial console open", "device", cfg.Serial.Device, "baud", cfg.Serial.Baud)

	// Power control (optional).
	var pwr power.Controller
	var powerOutlet int32 // 0 = power control unconfigured; surfaced by GetStatus
	if cfg.Power.Address != "" {
		powerOutlet = int32(cfg.Power.Outlet)
		apc, err := power.NewAPC(power.APCConfig{
			Address:        cfg.Power.Address,
			Community:      cfg.Power.Community,
			Outlet:         cfg.Power.Outlet,
			Version:        cfg.Power.Version,
			Timeout:        cfg.Power.Timeout,
			Retries:        cfg.Power.Retries,
			OffDuration:    cfg.Power.OffDuration,
			CommandOIDBase: cfg.Power.CommandOIDBase,
			StateOIDBase:   cfg.Power.StateOIDBase,
		})
		if err != nil {
			return fmt.Errorf("configure power: %w", err)
		}
		// Clear the console scrollback whenever power is removed. Without this the
		// 1 MiB ring accumulates every boot, so reading the log after a power cycle
		// shows a confusing stack of previous boots instead of just the fresh one.
		pwr = power.Hook(apc, console.Reset)
		logger.Info("power control configured", "pdu", cfg.Power.Address, "outlet", cfg.Power.Outlet)
	} else {
		logger.Warn("power control not configured (no power.address); power-cycle disabled")
	}

	// U-Boot orchestrator.
	orch, err := uboot.New(console, pwr, uboot.Config{
		ServerIP:         cfg.UBoot.ServerIP,
		EthAct:           cfg.UBoot.EthAct,
		LoadAddr:         cfg.UBoot.LoadAddr,
		CoreMask:         cfg.UBoot.CoreMask,
		PromptPattern:    cfg.UBoot.PromptPattern,
		InterruptPattern: cfg.UBoot.InterruptPattern,
		InterruptKey:     cfg.UBoot.InterruptKey,
		KernelBanner:     cfg.UBoot.KernelBanner,
		CommandTimeout:   cfg.UBoot.CommandTimeout,
	})
	if err != nil {
		return fmt.Errorf("configure uboot: %w", err)
	}

	// Image store + TFTP server. The store is created whenever an image
	// directory is set, independent of whether this instance runs the TFTP
	// server: on a shared-TFTP controller one instance serves UDP/69 while the
	// others still accept uploads into the shared directory. The gRPC service
	// uses a per-instance namespaced view (images stored under a name prefix)
	// so concurrent DUTs never clobber each other; the TFTP read server uses the
	// raw store so it serves every instance's files by their exact on-disk name.
	var store *tftp.Store
	var tftpSrv *tftp.Server
	if cfg.TFTP.Dir != "" {
		rawStore, err := tftp.NewStore(cfg.TFTP.Dir)
		if err != nil {
			return fmt.Errorf("image store: %w", err)
		}
		store = rawStore.Namespaced(cfg.Name)
		if cfg.TFTPEnabled() {
			tftpSrv = tftp.NewServer(rawStore, cfg.TFTP.Address, logger)
			go func() {
				if err := tftpSrv.ListenAndServe(); err != nil {
					logger.Error("tftp server stopped", "err", err)
				}
			}()
			defer tftpSrv.Shutdown()
		}
	}

	// SSH client (optional).
	var ssh *sshexec.Client
	if cfg.SSH.Host != "" {
		ssh, err = sshexec.New(sshexec.Config{
			Host:           cfg.SSH.Host,
			User:           cfg.SSH.User,
			Password:       cfg.SSH.Password,
			PrivateKeyPath: cfg.SSH.PrivateKeyPath,
			KnownHostsPath: cfg.SSH.KnownHostsPath,
			DialTimeout:    cfg.SSH.DialTimeout,
		})
		if err != nil {
			return fmt.Errorf("configure ssh: %w", err)
		}
		logger.Info("ssh configured", "host", cfg.SSH.Host, "user", cfg.SSH.User)
	}

	// Session locking (single-user hardware lock).
	var sessions *session.Manager
	if cfg.SessionEnabled() {
		sessions = session.NewManager(cfg.Session.TTL)
		defer sessions.Close()
		logger.Info("session locking enabled", "ttl", sessions.TTL())
	} else {
		logger.Warn("session locking DISABLED; concurrent clients can conflict")
	}

	svc := server.New(server.Deps{
		Version:      version,
		Console:      console,
		Power:        pwr,
		Orchestrator: orch,
		Store:        store,
		SSH:          ssh,
		Sessions:     sessions,
		Logger:       logger,
		SerialDevice: cfg.Serial.Device,
		SerialBaud:   uint32(cfg.Serial.Baud),
		TFTPAddress:  cfg.TFTP.Address,
		SSHTarget:    cfg.SSH.Host,
		SSHUser:      cfg.SSH.User,
		PowerOutlet:  powerOutlet,
	})

	// gRPC server with optional TLS.
	var opts []grpc.ServerOption
	if cfg.TLSEnabled() {
		creds, err := tlsutil.ServerCredentials(tlsutil.ServerOptions{
			CertFile:     cfg.GRPC.TLS.CertFile,
			KeyFile:      cfg.GRPC.TLS.KeyFile,
			ClientCAFile: cfg.GRPC.TLS.ClientCAFile,
		})
		if err != nil {
			return err
		}
		opts = append(opts, grpc.Creds(creds))
		mtls := cfg.GRPC.TLS.ClientCAFile != ""
		logger.Info("grpc TLS enabled", "mutual_tls", mtls)
	} else {
		logger.Warn("grpc TLS DISABLED; do not expose this port to untrusted networks")
	}

	opts = append(opts,
		grpc.ChainUnaryInterceptor(svc.UnaryInterceptor),
		grpc.ChainStreamInterceptor(svc.StreamInterceptor),
	)
	grpcSrv := grpc.NewServer(opts...)
	unwedgev1.RegisterUnwedgeServer(grpcSrv, svc)

	lis, err := net.Listen("tcp", cfg.GRPC.Address)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.GRPC.Address, err)
	}
	logger.Info("unwedged listening", "addr", cfg.GRPC.Address, "version", version)

	// Graceful shutdown on signal.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- grpcSrv.Serve(lis) }()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
		grpcSrv.GracefulStop()
		return nil
	case err := <-serveErr:
		return err
	}
}
