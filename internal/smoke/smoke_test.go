package smoke

import (
	"context"
	"regexp"
	"testing"
	"time"
)

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
