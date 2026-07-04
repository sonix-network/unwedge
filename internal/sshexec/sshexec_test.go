package sshexec

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// startTestSSHServer starts a minimal SSH server that, for each exec request,
// echoes a canned reply derived from the command and returns exitCodeFor(cmd).
func startTestSSHServer(t *testing.T, exitCodeFor func(cmd string) (stdout, stderr string, code int)) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	signer, err := ssh.NewSignerFromSigner(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			nConn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(nConn, cfg, exitCodeFor)
		}
	}()
	return ln.Addr().String()
}

func serveConn(nConn net.Conn, cfg *ssh.ServerConfig, exitCodeFor func(string) (string, string, int)) {
	sconn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				if req.Type != "exec" {
					if req.WantReply {
						req.Reply(false, nil)
					}
					continue
				}
				var payload struct{ Command string }
				ssh.Unmarshal(req.Payload, &payload)
				if req.WantReply {
					req.Reply(true, nil)
				}
				stdout, stderr, code := exitCodeFor(payload.Command)
				ch.Write([]byte(stdout))
				ch.Stderr().Write([]byte(stderr))
				status := make([]byte, 4)
				binary.BigEndian.PutUint32(status, uint32(code))
				ch.SendRequest("exit-status", false, status)
				ch.Close()
			}
		}()
	}
}

func TestSSHExecSuccess(t *testing.T) {
	addr := startTestSSHServer(t, func(cmd string) (string, string, int) {
		return "uname output: Linux\n", "", 0
	})
	c, err := New(Config{Host: addr, User: "root", Password: "x"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := c.Exec(context.Background(), "uname -a", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d", res.ExitCode)
	}
	if !strings.Contains(string(res.Stdout), "Linux") {
		t.Fatalf("stdout = %q", res.Stdout)
	}
}

func TestSSHExecNonZeroExit(t *testing.T) {
	addr := startTestSSHServer(t, func(cmd string) (string, string, int) {
		return "", "boom\n", 42
	})
	c, _ := New(Config{Host: addr, User: "root", Password: "x"})
	res, err := c.Exec(context.Background(), "false", 5*time.Second)
	if err != nil {
		t.Fatalf("Exec returned transport error: %v", err)
	}
	if res.ExitCode != 42 {
		t.Fatalf("exit = %d, want 42", res.ExitCode)
	}
	if !strings.Contains(string(res.Stderr), "boom") {
		t.Fatalf("stderr = %q", res.Stderr)
	}
}

func TestSSHExecNoAuth(t *testing.T) {
	c, _ := New(Config{Host: "127.0.0.1:22", User: "root"})
	if _, err := c.Exec(context.Background(), "x", time.Second); err == nil {
		t.Fatal("expected error with no auth configured")
	}
}
