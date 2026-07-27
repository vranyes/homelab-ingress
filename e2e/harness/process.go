package harness

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ManagedProcess wraps an os/exec.Cmd with lifecycle helpers for the test
// harness. Processes are started with Spawn and stopped with Terminate.
type ManagedProcess struct {
	Cmd  *exec.Cmd
	done chan error
}

// Spawn starts a binary at path with the given args and environment
// variables. stdout and stderr are appended to logPath (if non-empty) or
// inherited from the test process. Spawn does not block — the caller should
// use the service-specific WaitFor* helpers (WaitForIngress, etc.) to
// confirm readiness.
func Spawn(path string, args []string, env []string, logPath string) (*ManagedProcess, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = append(os.Environ(), env...)

	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, fmt.Errorf("open log %s: %w", logPath, err)
		}
		cmd.Stdout = f
		cmd.Stderr = f
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", path, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	return &ManagedProcess{Cmd: cmd, done: done}, nil
}

// Terminate sends SIGTERM and waits up to timeout for the process to exit.
// If the process is still alive after timeout it is killed with SIGKILL.
// It is safe to call Terminate on a nil or already-exited process.
func (p *ManagedProcess) Terminate(timeout time.Duration) error {
	if p == nil || p.Cmd == nil || p.Cmd.Process == nil {
		return nil
	}

	if err := p.Cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal SIGTERM: %w", err)
	}

	select {
	case err := <-p.done:
		return err
	case <-time.After(timeout):
		if err := p.Cmd.Process.Kill(); err != nil {
			return fmt.Errorf("kill after timeout: %w", err)
		}
		return <-p.done
	}
}

// Signal sends an arbitrary signal to the process. It is a thin wrapper
// around Process.Signal for tests that need to simulate a crash (e.g.
// syscall.SIGKILL).
func (p *ManagedProcess) Signal(sig os.Signal) error {
	if p == nil || p.Cmd == nil || p.Cmd.Process == nil {
		return nil
	}
	return p.Cmd.Process.Signal(sig)
}
