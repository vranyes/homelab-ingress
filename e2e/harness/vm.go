// Package harness contains helpers for the homelab-ingress Ginkgo e2e suite.
//
// The test binary runs inside the lima VM as root. These helpers wrap shell
// commands (executed via `sh -c`, not via limactl), file IO, and OpenRC service
// supervision. They intentionally avoid Ginkgo/Gomega imports so they can be
// invoked from any Go context, including ad-hoc debugging utilities.
package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// RunShell executes `sh -c "<script>"` and returns the combined stdout/stderr.
func RunShell(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	out, err := cmd.CombinedOutput()
	return out, err
}

// RunShellLines joins the given lines with newlines and runs them as a single
// shell script.
func RunShellLines(ctx context.Context, lines ...string) ([]byte, error) {
	return RunShell(ctx, strings.Join(lines, "\n"))
}

// StartService starts an OpenRC service. "already started" is treated as
// success.
func StartService(ctx context.Context, name string) error {
	out, err := RunShell(ctx, fmt.Sprintf("rc-service %s start", name))
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)), "already started") {
			return nil
		}
		return fmt.Errorf("rc-service %s start: %w: %s", name, err, string(out))
	}
	return nil
}

// StopService stops an OpenRC service. "not started" / "service not running"
// are treated as success.
func StopService(ctx context.Context, name string) error {
	out, err := RunShell(ctx, fmt.Sprintf("rc-service %s stop", name))
	if err != nil {
		lowered := strings.ToLower(string(out))
		if strings.Contains(lowered, "not started") || strings.Contains(lowered, "not running") {
			return nil
		}
		return fmt.Errorf("rc-service %s stop: %w: %s", name, err, string(out))
	}
	return nil
}

// RestartService restarts an OpenRC service.
func RestartService(ctx context.Context, name string) error {
	out, err := RunShell(ctx, fmt.Sprintf("rc-service %s restart", name))
	if err != nil {
		return fmt.Errorf("rc-service %s restart: %w: %s", name, err, string(out))
	}
	return nil
}

// ServiceStatus returns the output of `rc-service <name> status`.
func ServiceStatus(ctx context.Context, name string) (string, error) {
	out, err := RunShell(ctx, fmt.Sprintf("rc-service %s status", name))
	return strings.TrimSpace(string(out)), err
}

// KillService hard-kills the underlying binary for an OpenRC service via
// `pkill -9 -f <binaryName>` and then waits for ServiceStatus to stop
// reporting "running". OpenRC's supervise-daemon should respawn the process
// shortly afterwards.
func KillService(ctx context.Context, binaryName string) error {
	if _, err := RunShell(ctx, fmt.Sprintf("pkill -9 -f %s || true", binaryName)); err != nil {
		return fmt.Errorf("pkill %s: %w", binaryName, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, _ := ServiceStatus(ctx, strings.ReplaceAll(binaryName, "/usr/local/bin/", ""))
		if strings.Contains(strings.ToLower(status), "stopped") ||
			strings.Contains(strings.ToLower(status), "crashed") ||
			strings.Contains(strings.ToLower(status), "inactive") {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

// FileExists reports whether the given path exists.
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// WaitForFile polls for the existence of `path` until `timeout` elapses.
func WaitForFile(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if FileExists(path) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("timeout waiting for file %s after %s", path, timeout)
}

// WriteFile writes `contents` to `path` with the given mode, creating parent
// directories as needed.
func WriteFile(path string, contents []byte, mode os.FileMode) error {
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return fmt.Errorf("mkdir parent of %s: %w", path, err)
	}
	return os.WriteFile(path, contents, mode)
}

// ReadFile reads the contents of `path`.
func ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// AppendFile opens `path` in append mode (creating it if needed) and writes
// `contents` to the end.
func AppendFile(path string, contents []byte) error {
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return fmt.Errorf("mkdir parent of %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(contents)
	return err
}

func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		if i == 0 {
			return "/"
		}
		return path[:i]
	}
	return "."
}
