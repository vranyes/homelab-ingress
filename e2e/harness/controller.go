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
// controller/cmd/controller/main.go).
var ControllerReadyPattern = regexp.MustCompile(`starting manager`)

// controllerStartupSettle is how long the controller must survive after
// logging ControllerReadyPattern before it is considered ready. "starting
// manager" is emitted before controller-runtime binds its servers, so a
// listener conflict (e.g. metrics and health probe on the same port) logs the
// line and then exits within milliseconds.
const controllerStartupSettle = 750 * time.Millisecond

// WaitForControllerReady polls `/var/log/hl-ingress/controller.log` until a
// NEW line matching ControllerReadyPattern appears and the process survives
// the startup settle window, or `timeout` elapses.
//
// It records the current file size before polling so it only matches lines
// written after this call. The process handle is part of the contract because
// a log match alone can report a controller that has already died as ready;
// an early exit is returned as an error carrying the log tail.
func WaitForControllerReady(ctx context.Context, proc *ManagedProcess, logPath string, timeout time.Duration) error {
	fi, err := os.Stat(logPath)
	if err != nil {
		return fmt.Errorf("stat controller log: %w", err)
	}
	minSize := fi.Size()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, exited := proc.ExitErr(); exited {
			return controllerExitError(proc, logPath)
		}
		line, offset, err := GrepFileAfter(logPath, ControllerReadyPattern.String(), minSize)
		if err == nil && line != "" {
			minSize = offset
			return waitControllerSurvives(ctx, proc, logPath)
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

// waitControllerSurvives waits out the startup settle window, failing fast if
// the controller exits during it.
func waitControllerSurvives(ctx context.Context, proc *ManagedProcess, logPath string) error {
	select {
	case <-proc.exited:
		return controllerExitError(proc, logPath)
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(controllerStartupSettle):
		return nil
	}
}

// controllerExitError describes an unexpected controller exit and includes
// the tail of its log, which normally names the cause (for example a bind
// error) that would otherwise be buried in a 40-minute suite run.
func controllerExitError(proc *ManagedProcess, logPath string) error {
	exitErr, _ := proc.ExitErr()
	tail, err := TailFile(logPath, 15)
	if err != nil {
		return fmt.Errorf("controller exited before becoming ready (%v); reading log failed: %w", exitErr, err)
	}
	if tail == "" {
		tail = "<empty>"
	}
	return fmt.Errorf("controller exited before becoming ready (%v); last log lines:\n%s", exitErr, tail)
}
