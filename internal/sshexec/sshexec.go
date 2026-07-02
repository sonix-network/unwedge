// Package sshexec runs commands on the booted target over SSH. It dials per
// command so it tolerates the target rebooting between calls (expected during
// kernel development), and by default accepts any host key because a freshly
// netbooted image presents a new key every boot.
package sshexec

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
)

// Config configures the SSH client.
type Config struct {
	// Host is the target address (host or host:port). Port defaults to 22.
	Host string
	// User to authenticate as (root on OpenWrt).
	User string
	// Password authentication (optional).
	Password string
	// PrivateKeyPath / PrivateKeyPEM provide key authentication (optional).
	PrivateKeyPath string
	PrivateKeyPEM  []byte
	// KnownHostsPath enables host-key verification against this file. If empty,
	// host keys are accepted unverified (appropriate for a dev target whose key
	// changes every netboot).
	KnownHostsPath string
	// DialTimeout bounds establishing the TCP+SSH connection. 0 -> 10s.
	DialTimeout time.Duration
}

// Result is the outcome of an SSHExec.
type Result struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	TimedOut bool
}

// Client runs commands against a target.
type Client struct {
	cfg Config
}

// New validates config and returns a Client.
func New(cfg Config) (*Client, error) {
	if cfg.User == "" {
		return nil, fmt.Errorf("sshexec: user required")
	}
	return &Client{cfg: cfg}, nil
}

func (c *Client) authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if len(c.cfg.PrivateKeyPEM) > 0 || c.cfg.PrivateKeyPath != "" {
		pem := c.cfg.PrivateKeyPEM
		if len(pem) == 0 {
			b, err := os.ReadFile(c.cfg.PrivateKeyPath)
			if err != nil {
				return nil, fmt.Errorf("sshexec: read key: %w", err)
			}
			pem = b
		}
		signer, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			return nil, fmt.Errorf("sshexec: parse key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if c.cfg.Password != "" {
		methods = append(methods, ssh.Password(c.cfg.Password))
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("sshexec: no auth configured (set password or private key)")
	}
	return methods, nil
}

func (c *Client) hostKeyCallback() (ssh.HostKeyCallback, error) {
	if c.cfg.KnownHostsPath == "" {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	return knownHostsCallback(c.cfg.KnownHostsPath)
}

func withPort(host string, def string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, def)
}

// Exec runs command on host (or hostOverride if non-empty) and returns its
// output and exit status. A non-zero exit code is returned in Result, not as an
// error; err is reserved for connection/transport failures.
func (c *Client) Exec(ctx context.Context, hostOverride, command string, timeout time.Duration) (Result, error) {
	host := c.cfg.Host
	if hostOverride != "" {
		host = hostOverride
	}
	if host == "" {
		return Result{}, fmt.Errorf("sshexec: no host configured")
	}
	addr := withPort(host, "22")

	methods, err := c.authMethods()
	if err != nil {
		return Result{}, err
	}
	hkcb, err := c.hostKeyCallback()
	if err != nil {
		return Result{}, err
	}
	dialTimeout := c.cfg.DialTimeout
	if dialTimeout == 0 {
		dialTimeout = 10 * time.Second
	}
	sshCfg := &ssh.ClientConfig{
		User:            c.cfg.User,
		Auth:            methods,
		HostKeyCallback: hkcb,
		Timeout:         dialTimeout,
	}

	// Apply an overall context deadline covering dial + run.
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return Result{}, fmt.Errorf("sshexec: dial %s: %w", addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, sshCfg)
	if err != nil {
		conn.Close()
		return Result{}, fmt.Errorf("sshexec: handshake %s: %w", addr, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return Result{}, fmt.Errorf("sshexec: new session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	// Close the session if the context is cancelled, unblocking Wait.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			session.Close()
		case <-done:
		}
	}()

	res := Result{}
	runErr := session.Run(command)
	res.Stdout = stdout.Bytes()
	res.Stderr = stderr.Bytes()

	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return res, nil
	}
	if runErr == nil {
		res.ExitCode = 0
		return res, nil
	}
	if exitErr, ok := runErr.(*ssh.ExitError); ok {
		res.ExitCode = exitErr.ExitStatus()
		return res, nil
	}
	if _, ok := runErr.(*ssh.ExitMissingError); ok {
		// Session ended without exit status (e.g. killed by our ctx cancel).
		if ctx.Err() != nil {
			res.TimedOut = true
			return res, nil
		}
	}
	return res, fmt.Errorf("sshexec: run %q: %w", command, runErr)
}
