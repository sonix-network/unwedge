package sshexec

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// SCPMeta describes a file transferred over the classic scp protocol.
type SCPMeta struct {
	Size int64
	Mode os.FileMode
	Name string
}

// SCPUpload copies size bytes from src to remotePath on the target using the
// classic scp protocol (remote `scp -t`), so no SFTP subsystem is required.
// mode is the unix permission bits to request (0 -> 0644). remotePath may be a
// file path or an existing directory to drop the file into.
func (c *Client) SCPUpload(ctx context.Context, remotePath string, mode os.FileMode, size int64, src io.Reader, timeout time.Duration) error {
	if size < 0 {
		return fmt.Errorf("sshexec: negative size")
	}
	if mode == 0 {
		mode = 0o644
	}
	name := baseName(remotePath)

	return c.scpSession(ctx, "scp -t -- "+shellQuote(remotePath), timeout,
		func(w io.Writer, r *bufio.Reader) error {
			// The sink (remote scp -t) sends an initial ack when ready.
			if err := scpReadAck(r); err != nil {
				return err
			}
			// Announce the file: C<mode> <size> <name>\n.
			if _, err := fmt.Fprintf(w, "C%04o %d %s\n", mode.Perm(), size, name); err != nil {
				return err
			}
			if err := scpReadAck(r); err != nil {
				return err
			}
			if n, err := io.CopyN(w, src, size); err != nil {
				return fmt.Errorf("scp: send body (%d/%d bytes): %w", n, size, err)
			}
			// A trailing zero byte terminates the file, then a final ack.
			if _, err := w.Write([]byte{0}); err != nil {
				return err
			}
			return scpReadAck(r)
		})
}

// SCPDownload copies remotePath from the target using the classic scp protocol
// (remote `scp -f`) and hands the metadata and a reader positioned at the file
// body to sink. sink must consume exactly meta.Size bytes.
func (c *Client) SCPDownload(ctx context.Context, remotePath string, timeout time.Duration, sink func(meta SCPMeta, body io.Reader) error) error {
	return c.scpSession(ctx, "scp -f -- "+shellQuote(remotePath), timeout,
		func(w io.Writer, r *bufio.Reader) error {
			// Tell the source we are ready to receive.
			if _, err := w.Write([]byte{0}); err != nil {
				return err
			}
			// Read control lines until we get a file record. Skip timestamp
			// records; reject directory recursion (we only do single files).
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					if err == io.EOF && line == "" {
						return fmt.Errorf("scp: no file received (source produced no data)")
					}
					return fmt.Errorf("scp: read control: %w", err)
				}
				switch line[0] {
				case 'T': // timestamp record (-p); ack and continue.
					if _, err := w.Write([]byte{0}); err != nil {
						return err
					}
					continue
				case 0x01, 0x02: // warning / error from source.
					return fmt.Errorf("scp remote: %s", strings.TrimSpace(line[1:]))
				case 'D', 'E':
					return fmt.Errorf("scp: %q is a directory (recursive download unsupported)", remotePath)
				case 'C':
					meta, err := parseSCPFileLine(line)
					if err != nil {
						return err
					}
					if _, err := w.Write([]byte{0}); err != nil { // ack the record
						return err
					}
					if err := sink(meta, io.LimitReader(r, meta.Size)); err != nil {
						return err
					}
					// Source sends a status byte after the body; ack it.
					if err := scpReadAck(r); err != nil {
						return err
					}
					if _, err := w.Write([]byte{0}); err != nil {
						return err
					}
					return nil
				default:
					return fmt.Errorf("scp: unexpected control byte %#x", line[0])
				}
			}
		})
}

// scpSession opens an SSH session running command (a remote `scp -t/-f`) and
// drives the scp protocol via run, wiring run's writer to the remote stdin and
// its reader to the remote stdout. Remote stderr is captured for diagnostics.
func (c *Client) scpSession(ctx context.Context, command string, timeout time.Duration, run func(w io.Writer, r *bufio.Reader) error) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	client, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("sshexec: new session: %w", err)
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	session.Stderr = &stderr

	// Close the session if the context is cancelled, unblocking the protocol.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			session.Close()
		case <-done:
		}
	}()

	if err := session.Start(command); err != nil {
		return fmt.Errorf("sshexec: start %q: %w", command, err)
	}

	runErr := run(stdin, bufio.NewReader(stdout))
	// Signal EOF to the remote so it exits, then collect its status.
	stdin.Close()
	waitErr := session.Wait()

	if ctx.Err() != nil {
		return fmt.Errorf("scp: %w", ctx.Err())
	}
	if runErr != nil {
		return annotateSCPErr(runErr, stderr.Bytes())
	}
	if waitErr != nil {
		if _, ok := waitErr.(*ssh.ExitMissingError); ok {
			return nil // body transferred; remote closed without a status
		}
		return annotateSCPErr(fmt.Errorf("scp: remote exited: %w", waitErr), stderr.Bytes())
	}
	return nil
}

func annotateSCPErr(err error, stderr []byte) error {
	if s := strings.TrimSpace(string(stderr)); s != "" {
		return fmt.Errorf("%w (remote: %s)", err, s)
	}
	return err
}

// scpReadAck reads a single scp status byte: 0 = OK, 1 = warning, 2 = fatal.
// For non-zero statuses the remote follows with a newline-terminated message.
func scpReadAck(r *bufio.Reader) error {
	b, err := r.ReadByte()
	if err != nil {
		return fmt.Errorf("scp: read ack: %w", err)
	}
	switch b {
	case 0:
		return nil
	case 1, 2:
		msg, _ := r.ReadString('\n')
		return fmt.Errorf("scp remote: %s", strings.TrimSpace(msg))
	default:
		return fmt.Errorf("scp: unexpected ack byte %#x", b)
	}
}

// parseSCPFileLine parses a "C<mode> <size> <name>\n" record.
func parseSCPFileLine(line string) (SCPMeta, error) {
	fields := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
	if len(fields) != 3 || len(fields[0]) < 2 || fields[0][0] != 'C' {
		return SCPMeta{}, fmt.Errorf("scp: malformed file record %q", line)
	}
	mode, err := strconv.ParseUint(fields[0][1:], 8, 32)
	if err != nil {
		return SCPMeta{}, fmt.Errorf("scp: bad mode in %q: %w", line, err)
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return SCPMeta{}, fmt.Errorf("scp: bad size in %q: %w", line, err)
	}
	return SCPMeta{Size: size, Mode: os.FileMode(mode).Perm(), Name: fields[2]}, nil
}

// baseName returns the final path element of a slash-separated remote path,
// defaulting to "file" for empty or directory-like paths.
func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	if p == "" {
		return "file"
	}
	return p
}

// shellQuote single-quotes s for a POSIX remote shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
