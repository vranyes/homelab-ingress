package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests cover the process and readiness plumbing itself. They need no
// VM, no network and no root: they spawn /bin/sh scripts.

func TestManagedProcessAliveAndExitErr(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "out.log")

	proc, err := Spawn("/bin/sh", []string{"-c", "sleep 30"}, nil, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !proc.Alive() {
		t.Fatalf("process must be alive right after Spawn")
	}
	if _, exited := proc.ExitErr(); exited {
		t.Fatalf("ExitErr reported an exit for a running process")
	}
	// Terminate reports the signal-delivered exit as an error; what matters
	// here is that the process is gone and the exit is observable.
	_ = proc.Terminate(5 * time.Second)
	if proc.Alive() {
		t.Fatalf("process must not be alive after Terminate")
	}
	if _, exited := proc.ExitErr(); !exited {
		t.Fatalf("ExitErr must report the exit after Terminate")
	}
}

func TestManagedProcessExitErrReportsFailure(t *testing.T) {
	proc, err := Spawn("/bin/sh", []string{"-c", "exit 3"}, nil, filepath.Join(t.TempDir(), "out.log"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForExit(t, proc, 5*time.Second)
	if proc.Alive() {
		t.Fatalf("process must be reported dead after exiting")
	}
	exitErr, exited := proc.ExitErr()
	if !exited {
		t.Fatalf("ExitErr must report the exit")
	}
	if exitErr == nil {
		t.Fatalf("ExitErr must surface a non-zero exit status")
	}
}

// TestWaitForControllerReadyRejectsFastExit reproduces the failure mode that
// silently passed before: the manager logs "starting manager" and then dies
// (a port conflict does exactly this). The gate must return an error that
// includes the log tail instead of declaring readiness.
func TestWaitForControllerReadyRejectsFastExit(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "controller.log")
	script := `echo "setup INFO starting manager"; echo "setup ERROR problem running manager"; exit 1`
	proc, err := Spawn("/bin/sh", []string{"-c", script}, nil, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForExit(t, proc, 5*time.Second)

	err = WaitForControllerReady(context.Background(), proc, logPath, 5*time.Second)
	if err == nil {
		t.Fatalf("expected an error for a controller that exited after logging readiness")
	}
	if !strings.Contains(err.Error(), "problem running manager") {
		t.Fatalf("error must include the log tail, got: %v", err)
	}
}

// TestWaitForControllerReadyAcceptsSurvivingProcess is the positive control:
// a process that logs the readiness line and stays up must pass.
func TestWaitForControllerReadyAcceptsSurvivingProcess(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "controller.log")
	proc, err := Spawn("/bin/sh", []string{"-c", `echo "setup INFO starting manager"; sleep 30`}, nil, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = proc.Terminate(5 * time.Second) })

	if err := WaitForControllerReady(context.Background(), proc, logPath, 5*time.Second); err != nil {
		t.Fatalf("WaitForControllerReady: %v", err)
	}
}

// TestWaitForControllerReadyHintsAtSubsequentDeath covers the settle window:
// the line appears, the process lingers briefly, then dies before the settle
// window closes. That must still be a failure.
func TestWaitForControllerReadyHintsAtSubsequentDeath(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "controller.log")
	proc, err := Spawn("/bin/sh", []string{"-c", `echo "setup INFO starting manager"; sleep 0.2; exit 1`}, nil, logPath)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := WaitForControllerReady(context.Background(), proc, logPath, 5*time.Second); err == nil {
		t.Fatalf("expected an error for a controller that died inside the settle window")
	}
}

// TestWaitForControllerReadyTimesOutWithoutPattern pins the no-line path.
func TestWaitForControllerReadyTimesOutWithoutPattern(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "controller.log")
	if err := os.WriteFile(logPath, []byte("still booting\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	proc, err := Spawn("/bin/sh", []string{"-c", "sleep 30"}, nil, filepath.Join(t.TempDir(), "out.log"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = proc.Terminate(5 * time.Second) })

	start := time.Now()
	err = WaitForControllerReady(context.Background(), proc, logPath, 700*time.Millisecond)
	if err == nil {
		t.Fatalf("expected a timeout error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout was not honored (waited %s)", time.Since(start))
	}
}

func waitForExit(t *testing.T, proc *ManagedProcess, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !proc.Alive() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process did not exit within %s", timeout)
}
