package sshexec

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// scpTestServer is a minimal SSH server that emulates a remote `scp -t` (sink)
// and `scp -f` (source) so the client-side scp protocol can be exercised
// end-to-end. Uploaded files land in files, keyed by their remote path.
type scpTestServer struct {
	mu    sync.Mutex
	files map[string][]byte
}

func startSCPServer(t *testing.T, seed map[string][]byte) (string, *scpTestServer) {
	t.Helper()
	srv := &scpTestServer{files: map[string][]byte{}}
	for k, v := range seed {
		srv.files[k] = v
	}
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
			go srv.serve(nConn, cfg)
		}
	}()
	return ln.Addr().String(), srv
}

func (s *scpTestServer) serve(nConn net.Conn, cfg *ssh.ServerConfig) {
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
				code := s.runSCP(payload.Command, ch)
				status := make([]byte, 4)
				binary.BigEndian.PutUint32(status, uint32(code))
				ch.SendRequest("exit-status", false, status)
				ch.Close()
			}
		}()
	}
}

// runSCP emulates the remote scp binary for a single exec.
func (s *scpTestServer) runSCP(cmd string, ch ssh.Channel) int {
	r := bufio.NewReader(ch)
	switch {
	case strings.HasPrefix(cmd, "scp -t -- "):
		return s.sink(scpTestPath(cmd, "scp -t -- "), r, ch)
	case strings.HasPrefix(cmd, "scp -f -- "):
		return s.source(scpTestPath(cmd, "scp -f -- "), r, ch)
	default:
		fmt.Fprintf(ch.Stderr(), "unknown command %q\n", cmd)
		return 1
	}
}

// sink emulates `scp -t path`: receive one file and store it.
func (s *scpTestServer) sink(path string, r *bufio.Reader, ch ssh.Channel) int {
	ch.Write([]byte{0}) // initial ready ack
	line, err := r.ReadString('\n')
	if err != nil {
		return 1
	}
	var mode uint32
	var size int64
	var name string
	if _, err := fmt.Sscanf(line, "C%o %d %s", &mode, &size, &name); err != nil {
		fmt.Fprintf(ch.Stderr(), "bad C record %q\n", line)
		return 1
	}
	ch.Write([]byte{0}) // ack the record
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return 1
	}
	if _, err := r.ReadByte(); err != nil { // trailing status byte from source
		return 1
	}
	ch.Write([]byte{0}) // ack the body
	s.mu.Lock()
	s.files[path] = body
	s.mu.Unlock()
	return 0
}

// source emulates `scp -f path`: send one stored file.
func (s *scpTestServer) source(path string, r *bufio.Reader, ch ssh.Channel) int {
	if _, err := r.ReadByte(); err != nil { // client's initial ready ack
		return 1
	}
	s.mu.Lock()
	body, ok := s.files[path]
	s.mu.Unlock()
	if !ok {
		// Real scp -f reports errors inline as \x02<message>\n on the channel.
		fmt.Fprintf(ch, "\x02scp: %s: No such file or directory\n", path)
		return 1
	}
	fmt.Fprintf(ch, "C0644 %d %s\n", len(body), scpBase(path))
	if _, err := r.ReadByte(); err != nil { // ack of the record
		return 1
	}
	ch.Write(body)
	ch.Write([]byte{0}) // status after body
	r.ReadByte()        // final ack from client
	return 0
}

func scpTestPath(cmd, prefix string) string {
	arg := strings.TrimPrefix(cmd, prefix)
	arg = strings.TrimSpace(arg)
	arg = strings.TrimPrefix(arg, "'")
	arg = strings.TrimSuffix(arg, "'")
	return strings.ReplaceAll(arg, `'\''`, "'")
}

func scpBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func TestSCPUpload(t *testing.T) {
	addr, srv := startSCPServer(t, nil)
	c, err := New(Config{Host: addr, User: "root", Password: "x"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	content := bytes.Repeat([]byte("payload\n"), 1000) // 8000 bytes
	err = c.SCPUpload(context.Background(), "/tmp/dut.bin", 0o644, int64(len(content)),
		bytes.NewReader(content), 10*time.Second)
	if err != nil {
		t.Fatalf("SCPUpload: %v", err)
	}
	srv.mu.Lock()
	got := srv.files["/tmp/dut.bin"]
	srv.mu.Unlock()
	if !bytes.Equal(got, content) {
		t.Fatalf("uploaded %d bytes, want %d (equal=%v)", len(got), len(content), bytes.Equal(got, content))
	}
}

func TestSCPDownload(t *testing.T) {
	content := bytes.Repeat([]byte("abc123\n"), 500)
	addr, _ := startSCPServer(t, map[string][]byte{"/etc/data": content})
	c, _ := New(Config{Host: addr, User: "root", Password: "x"})

	var buf bytes.Buffer
	var meta SCPMeta
	err := c.SCPDownload(context.Background(), "/etc/data", 10*time.Second,
		func(m SCPMeta, body io.Reader) error {
			meta = m
			_, err := io.Copy(&buf, body)
			return err
		})
	if err != nil {
		t.Fatalf("SCPDownload: %v", err)
	}
	if meta.Size != int64(len(content)) {
		t.Fatalf("meta.Size = %d, want %d", meta.Size, len(content))
	}
	if meta.Name != "data" {
		t.Fatalf("meta.Name = %q, want data", meta.Name)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Fatalf("downloaded content mismatch (%d bytes)", buf.Len())
	}
}

func TestSCPDownloadMissing(t *testing.T) {
	addr, _ := startSCPServer(t, nil)
	c, _ := New(Config{Host: addr, User: "root", Password: "x"})
	err := c.SCPDownload(context.Background(), "/nope", 10*time.Second,
		func(SCPMeta, io.Reader) error { return nil })
	if err == nil {
		t.Fatal("expected error downloading a missing file")
	}
	if !strings.Contains(err.Error(), "No such file") {
		t.Fatalf("error = %v, want it to mention the remote message", err)
	}
}
