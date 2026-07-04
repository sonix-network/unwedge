package main

import (
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
)

// TestProxyIgnoresSIGPIPE guards the ssh -W lock leak: when the local ssh exits
// and closes the ProxyCommand pipe, the tunnel's write to os.Stdout hits EPIPE.
// Go raises SIGPIPE on writes to fd 1/2, whose default action kills the process
// before deferred cleanup (the hardware-lock release) runs. cmdSSH's proxy path
// calls signal.Ignore(syscall.SIGPIPE) so the broken pipe surfaces as a normal
// write error and deferred cleanup still runs.
//
// The kill only happens for a real fd-1 pipe, so this re-execs the test binary
// as a child whose stdout is a pipe we close, then checks that its deferred
// marker (stand-in for the lock release) reached stderr.
func TestProxyIgnoresSIGPIPE(t *testing.T) {
	for _, tc := range []struct {
		mode        string
		wantCleanup bool // did deferred cleanup run before exit?
	}{
		{"ignore", true},   // with the fix: write errors, cleanup runs
		{"default", false}, // without it: SIGPIPE kills the process, cleanup skipped
	} {
		t.Run(tc.mode, func(t *testing.T) {
			pr, pw, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			pr.Close() // reader gone: any write to the child's stdout gets EPIPE

			cmd := exec.Command(os.Args[0], "-test.run=TestSIGPIPEChild")
			cmd.Env = append(os.Environ(), "UNWEDGE_SIGPIPE_CHILD="+tc.mode)
			cmd.Stdout = pw
			var stderr strings.Builder
			cmd.Stderr = &stderr
			runErr := cmd.Run()
			pw.Close()

			gotCleanup := strings.Contains(stderr.String(), "CLEANUP-RAN")
			if gotCleanup != tc.wantCleanup {
				t.Fatalf("mode %q: cleanup ran = %v, want %v (child err %v, stderr %q)",
					tc.mode, gotCleanup, tc.wantCleanup, runErr, stderr.String())
			}
		})
	}
}

// TestSIGPIPEChild is the re-exec'd child for TestProxyIgnoresSIGPIPE. It is a
// no-op unless UNWEDGE_SIGPIPE_CHILD is set, so it stays inert in normal runs.
func TestSIGPIPEChild(t *testing.T) {
	mode := os.Getenv("UNWEDGE_SIGPIPE_CHILD")
	if mode == "" {
		return
	}
	// Deferred cleanup stands in for the deferred hardware-lock release; if
	// SIGPIPE kills us this never prints.
	defer func() {
		os.Stderr.WriteString("CLEANUP-RAN\n")
		os.Exit(0)
	}()
	if mode == "ignore" {
		signal.Ignore(syscall.SIGPIPE)
	}
	// Write until the broken pipe is noticed (as an error with the fix, or as a
	// fatal SIGPIPE without it).
	buf := make([]byte, 4096)
	for i := 0; i < 1000; i++ {
		if _, err := os.Stdout.Write(buf); err != nil {
			return // EPIPE surfaced as an error: fall through to deferred cleanup
		}
	}
}
