package harness

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"time"
)

// ControllerReadyPattern matches the line logged by the controller when it
// starts its manager: `setup	INFO	starting manager	...` (see
// controller/cmd/controller/main.go:114).
var ControllerReadyPattern = regexp.MustCompile(`starting manager`)

// ControllerReconciledPattern matches log lines emitted by the
// ServiceReconciler once it has created or updated a load balancer on the
// router. The controller logs `creating load balancer on router` and
// `updating load balancer on router` — we accept either.
var ControllerReconciledPattern = regexp.MustCompile(`(creating|updating load balancer on router|reconciled)`)

// WaitForControllerReady polls `/var/log/hl-ingress/controller.log` until a
// NEW line matching ControllerReadyPattern appears, or `timeout` elapses.
// It records the current file size before polling so it only matches lines
// written after this call.
func WaitForControllerReady(ctx context.Context, logPath string, timeout time.Duration) error {
	fi, err := os.Stat(logPath)
	if err != nil {
		return fmt.Errorf("stat controller log: %w", err)
	}
	minSize := fi.Size()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		line, offset, err := GrepFileAfter(logPath, ControllerReadyPattern.String(), minSize)
		if err == nil && line != "" {
			minSize = offset
			return nil
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("timeout waiting for controller ready after %s", timeout)
}

// WaitForControllerReconcile scans the controller log until a line matches
// ControllerReconciledPattern, or `timeout` elapses.
func WaitForControllerReconcile(ctx context.Context, logPath, serviceKey string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if line, _ := GrepFile(logPath, ControllerReconciledPattern.String()); line != "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("timeout waiting for controller reconcile of %s after %s", serviceKey, timeout)
}
