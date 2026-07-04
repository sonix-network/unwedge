package smoke

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	unwedgev1 "github.com/sonix-network/unwedge/gen/unwedge/v1"
)

// fakeSSH is a mock sshRunner: it fails (connection refused) for the first
// failUntil calls, then returns resp (or a default exit-0 response).
type fakeSSH struct {
	calls     int
	failUntil int
	resp      *unwedgev1.SSHExecResponse
}

func (f *fakeSSH) SSHExec(ctx context.Context, in *unwedgev1.SSHExecRequest, opts ...grpc.CallOption) (*unwedgev1.SSHExecResponse, error) {
	f.calls++
	if f.calls <= f.failUntil {
		return nil, errors.New("connection refused")
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &unwedgev1.SSHExecResponse{Stdout: []byte("ok")}, nil
}

func fastSSHRetry(t *testing.T) {
	t.Helper()
	old := sshRetryInterval
	sshRetryInterval = time.Millisecond
	t.Cleanup(func() { sshRetryInterval = old })
}

func TestTrySSHRetriesThenSucceeds(t *testing.T) {
	fastSSHRetry(t)
	f := &fakeSSH{failUntil: 2, resp: &unwedgev1.SSHExecResponse{Stdout: []byte("release X")}}
	resp, err := trySSH(context.Background(), f, "cat /etc/openwrt_release", time.Second)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := string(resp.GetStdout()); got != "release X" {
		t.Fatalf("stdout = %q", got)
	}
	if f.calls != 3 {
		t.Fatalf("calls = %d, want 3", f.calls)
	}
}

func TestTrySSHTimesOut(t *testing.T) {
	fastSSHRetry(t)
	f := &fakeSSH{failUntil: 1_000_000}
	if _, err := trySSH(context.Background(), f, "x", 30*time.Millisecond); err == nil {
		t.Fatal("expected a timeout error")
	}
}

func TestTrySSHNonzeroExitRetries(t *testing.T) {
	fastSSHRetry(t)
	// A non-zero exit is a failure that should be retried until timeout.
	f := &fakeSSH{resp: &unwedgev1.SSHExecResponse{ExitCode: 1, Stderr: []byte("no such file")}}
	if _, err := trySSH(context.Background(), f, "x", 20*time.Millisecond); err == nil {
		t.Fatal("expected failure on persistent non-zero exit")
	}
}

func TestCheckSSHExpectMatch(t *testing.T) {
	fastSSHRetry(t)
	cap := &captureBuf{}
	f := &fakeSSH{resp: &unwedgev1.SSHExecResponse{Stdout: []byte("DISTRIB_REVISION='SONIX-2026-07-04.7'\n")}}
	cfg := Config{SSHCommand: "cat /etc/openwrt_release", SSHExpect: `SONIX-2026-07-04\.7`, SSHTimeout: time.Second}
	out, ok, why := checkSSH(context.Background(), f, cfg, cap, "reached healthy userspace")
	if !ok {
		t.Fatalf("expected ok, got why=%q", why)
	}
	if !strings.Contains(out, "SONIX-2026-07-04.7") {
		t.Fatalf("out = %q", out)
	}
	if !strings.Contains(string(cap.bytes()), "ssh: cat /etc/openwrt_release") {
		t.Fatalf("boot log missing ssh output: %q", cap.bytes())
	}
}

func TestCheckSSHExpectMismatch(t *testing.T) {
	fastSSHRetry(t)
	cap := &captureBuf{}
	f := &fakeSSH{resp: &unwedgev1.SSHExecResponse{Stdout: []byte("SONIX-OLD\n")}}
	cfg := Config{SSHCommand: "cat /etc/openwrt_release", SSHExpect: "SONIX-NEW", SSHTimeout: time.Second}
	_, ok, why := checkSSH(context.Background(), f, cfg, cap, "reached")
	if ok {
		t.Fatal("expected mismatch to fail")
	}
	if !strings.Contains(why, "did not match") {
		t.Fatalf("why = %q", why)
	}
}

func TestCheckSSHConnectionFailure(t *testing.T) {
	fastSSHRetry(t)
	cap := &captureBuf{}
	f := &fakeSSH{failUntil: 1_000_000}
	cfg := Config{SSHCommand: "true", SSHTimeout: 20 * time.Millisecond}
	_, ok, why := checkSSH(context.Background(), f, cfg, cap, "reached")
	if ok {
		t.Fatal("expected ssh failure")
	}
	if !strings.Contains(why, "ssh check failed") {
		t.Fatalf("why = %q", why)
	}
}

func TestWaitForMarkerSuccess(t *testing.T) {
	cap := &captureBuf{}
	successRE := regexp.MustCompile(DefaultSuccessPattern)
	failureRE := regexp.MustCompile(DefaultFailurePattern)

	go func() {
		time.Sleep(100 * time.Millisecond)
		cap.write([]byte("[   19.4] init: Console is alive\n"))
		time.Sleep(100 * time.Millisecond)
		cap.write([]byte("procd: - init -\nPlease press Enter to activate this console\n"))
	}()

	ok, reason := waitForMarker(context.Background(), cap, successRE, failureRE, 3*time.Second)
	if !ok {
		t.Fatalf("expected success, got %q", reason)
	}
}

func TestWaitForMarkerFailure(t *testing.T) {
	cap := &captureBuf{}
	successRE := regexp.MustCompile(DefaultSuccessPattern)
	failureRE := regexp.MustCompile(DefaultFailurePattern)

	go func() {
		time.Sleep(100 * time.Millisecond)
		cap.write([]byte("[   2.1] Kernel panic - not syncing: Attempted to kill init!\n"))
	}()

	ok, reason := waitForMarker(context.Background(), cap, successRE, failureRE, 3*time.Second)
	if ok {
		t.Fatalf("expected failure, got success: %q", reason)
	}
	if want := "detected boot failure"; reason[:len(want)] != want {
		t.Fatalf("reason = %q", reason)
	}
}

func TestWaitForMarkerTimeout(t *testing.T) {
	cap := &captureBuf{}
	cap.write([]byte("booting but nothing interesting\n"))
	successRE := regexp.MustCompile(DefaultSuccessPattern)
	failureRE := regexp.MustCompile(DefaultFailurePattern)

	ok, reason := waitForMarker(context.Background(), cap, successRE, failureRE, 300*time.Millisecond)
	if ok {
		t.Fatalf("expected timeout failure")
	}
	if want := "timed out"; reason[:len(want)] != want {
		t.Fatalf("reason = %q", reason)
	}
}

func TestWaitForMarkerFailureBeatsSuccess(t *testing.T) {
	// If both appear, failure in earlier text should be caught first via ordering
	// of checks; ensure a panic anywhere fails the run.
	cap := &captureBuf{}
	cap.write([]byte("Please press Enter to activate this console\n" +
		"later: Kernel panic - not syncing\n"))
	successRE := regexp.MustCompile(DefaultSuccessPattern)
	failureRE := regexp.MustCompile(DefaultFailurePattern)
	ok, _ := waitForMarker(context.Background(), cap, successRE, failureRE, time.Second)
	if ok {
		t.Fatal("expected failure to take precedence when a panic is present")
	}
}

func TestDefaultPatternsCompile(t *testing.T) {
	if _, err := regexp.Compile(DefaultSuccessPattern); err != nil {
		t.Fatal(err)
	}
	if _, err := regexp.Compile(DefaultFailurePattern); err != nil {
		t.Fatal(err)
	}
}
