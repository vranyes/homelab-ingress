// Package harness contains helpers for the homelab-ingress Ginkgo e2e suite.
//
// The test binary runs inside the lima VM as root. These helpers wrap shell
// commands (executed via `sh -c`, not via limactl) and file IO.
// They intentionally avoid Ginkgo/Gomega imports so they can be invoked from
// any Go context, including ad-hoc debugging utilities.
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

// EnsureVepaRelayUp is the e2e suite's preflight check for the VEPA hairpin
// relay that the lima.yaml provisioning stands up. It is strictly a reader:
// the relay is created by /etc/init.d/hl-vepa-relay (which in turn uses
// `ip netns` + `tc mirred`) and dnsmasq runs inside that namespace via
// /etc/init.d/hl-dnsmasq-ns. This helper verifies all of the pieces are in
// place before any test runs and, on failure, dumps diagnostic state so the
// developer does not have to wait for a 60-second `WaitForServiceStatusIP`
// timeout to learn the relay is busted.
//
// On success, returns nil. On failure, returns an error whose message
// embeds the full `ip netns exec ns-server ...` diagnostic dump.
func EnsureVepaRelayUp(ctx context.Context) error {
	const (
		netns   = "ns-server"
		vouter  = "veth-outer"
		vinner  = "veth-inner"
		bridge  = "hl-br0"
		logPath = "/var/log/dnsmasq-ns.log"
	)

	// `out` and `err` are declared once and reused across the checks below.
	// Earlier iterations shadowed them with `:=` inside `if` initializers,
	// which silently dropped the per-step output once the if-block went out
	// of scope and caused the post-block `strings.Contains(string(out), ...)`
	// to keep testifying against the netns-list output from step 1.
	var (
		out []byte
		err error
	)

	// 1) netns exists.
	out, err = RunShell(ctx, "ip netns list | awk '{print $1}'")
	if err != nil {
		return fmt.Errorf("ip netns list: %w: %s", err, string(out))
	}
	if !containsLine(string(out), netns) {
		return relayError(ctx, fmt.Sprintf("netns %q missing — re-run `make start-e2e-vm`", netns))
	}

	// 2) veth-outer is present and UP inside the namespace.
	out, err = RunShell(ctx, fmt.Sprintf("ip -n %s -o link show %s", netns, vouter))
	if err != nil {
		return relayError(ctx, fmt.Sprintf("%s missing in %s: %s", vouter, netns, string(out)))
	}
	if !strings.Contains(string(out), "UP") {
		return relayError(ctx, fmt.Sprintf("%s is not UP in %s: %s", vouter, netns, string(out)))
	}

	// 3) veth-inner is enslaved to hl-br0 and UP in the root ns.
	out, err = RunShell(ctx, fmt.Sprintf("ip -o link show %s", vinner))
	if err != nil {
		return relayError(ctx, fmt.Sprintf("%s missing in root ns: %s", vinner, string(out)))
	}
	if !strings.Contains(string(out), "UP") {
		return relayError(ctx, fmt.Sprintf("%s is not UP in root ns: %s", vinner, string(out)))
	}
	out, err = RunShell(ctx, fmt.Sprintf("bridge link show dev %s", vinner))
	if err != nil || !strings.Contains(string(out), bridge) {
		return relayError(ctx, fmt.Sprintf("%s not enslaved to %s (bridge link show: %s, err=%v)", vinner, bridge, string(out), err))
	}

	// 4) The tc mirred bounceback filter is installed on veth-outer. We
	//    specifically assert `Egress Mirror` (not `Egress Redirect`):
	//    `redirect` marks the skb stolen and skips local delivery, so
	//    dnsmasq's UDP socket never sees the broadcast and DHCP silently
	//    times out. A relay misconfigured with `redirect` would otherwise
	//    pass every other check here and only fail as a 60s test timeout.
	out, err = RunShell(ctx, fmt.Sprintf("tc -n %s filter show dev %s ingress", netns, vouter))
	if err != nil {
		return relayError(ctx, fmt.Sprintf("tc filter show failed: %v: %s", err, string(out)))
	}
	if !strings.Contains(string(out), "mirred") {
		return relayError(ctx, fmt.Sprintf("tc mirred filter missing on %s in %s (tc output: %s)", vouter, netns, string(out)))
	}
	if !strings.Contains(string(out), "Egress Mirror") {
		return relayError(ctx, fmt.Sprintf(
			"tc mirred on %s in %s is not `Egress Mirror` — likely configured as `redirect`, "+
				"which steals the skb and prevents dnsmasq from receiving broadcasts. "+
				"Re-provision the VM with the latest e2e-vm/lima.yaml. (tc output: %s)",
			vouter, netns, string(out)))
	}

	// 5) dnsmasq is running inside the namespace and has bound to UDP/67.
	//    We check via ss(8) inside the netns because the listening socket
	//    exists only in ns-server's per-netns hash table.
	out, err = RunShell(ctx, fmt.Sprintf("ip netns exec %s ss -lun '( sport = 67 )'", netns))
	if err != nil {
		// ss returns non-zero when no socket matches; treat as relay failure.
		return relayError(ctx, fmt.Sprintf("ss UDP/67 probe failed inside %s: %s", netns, string(out)))
	}
	if !strings.Contains(string(out), ":67") {
		// dnsmasq may still be starting — give it a short grace window.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
			out, _ = RunShell(ctx, fmt.Sprintf("ip netns exec %s ss -lun '( sport = 67 )'", netns))
			if strings.Contains(string(out), ":67") {
				break
			}
		}
		if !strings.Contains(string(out), ":67") {
			return relayError(ctx, fmt.Sprintf("dnsmasq not listening on UDP/67 inside %s — check %s and the hl-dnsmasq-ns service", netns, logPath))
		}
	}

	return nil
}

// relayError builds a detailed error string that embeds the most-relevant
// diagnostic dumps so the suite failure message is actionable straight from
// the Ginkgo output, rather than forcing a developer to shell into the VM
// and re-run probes.
func relayError(ctx context.Context, msg string) error {
	dump := []string{
		"--- VEPA relay diagnostic dump ---",
		msg,
		"",
		"== ip netns list ==",
	}
	if out, err := RunShell(ctx, "ip netns list"); err == nil {
		dump = append(dump, strings.TrimSpace(string(out)))
	} else {
		dump = append(dump, fmt.Sprintf("(failed: %v)", err))
	}
	dump = append(dump, "", "== root-ns links (hl-br0, veth-*) ==")
	if out, err := RunShell(ctx, "ip -o link show | grep -E 'hl-br0|veth-' || true"); err == nil {
		dump = append(dump, strings.TrimSpace(string(out)))
	} else {
		dump = append(dump, fmt.Sprintf("(failed: %v)", err))
	}
	dump = append(dump, "", "== ns-server links + addrs ==")
	if out, err := RunShell(ctx, "ip -n ns-server -br addr"); err == nil {
		dump = append(dump, strings.TrimSpace(string(out)))
	} else {
		dump = append(dump, fmt.Sprintf("(failed: %v)", err))
	}
	dump = append(dump, "", "== ns-server tc qdisc + filters on veth-outer ==")
	if out, err := RunShell(ctx, "tc -n ns-server qdisc show dev veth-outer; tc -n ns-server filter show dev veth-outer ingress"); err == nil {
		dump = append(dump, strings.TrimSpace(string(out)))
	} else {
		dump = append(dump, fmt.Sprintf("(failed: %v)", err))
	}
	dump = append(dump, "", "== OpenRC status ==")
	if out, err := RunShell(ctx, "rc-service hl-br0 status; rc-service hl-vepa-relay status; rc-service hl-dnsmasq-ns status"); err == nil {
		dump = append(dump, strings.TrimSpace(string(out)))
	} else {
		dump = append(dump, fmt.Sprintf("(command failed: %v)", err))
	}
	dump = append(dump, "", "== tail /var/log/dnsmasq-ns.log ==")
	if out, err := RunShell(ctx, "tail -n 20 /var/log/dnsmasq-ns.log 2>/dev/null || true"); err == nil {
		dump = append(dump, strings.TrimSpace(string(out)))
	}
	dump = append(dump, "", "== tail /var/log/dnsmasq-ns.openrc.log ==")
	if out, err := RunShell(ctx, "tail -n 20 /var/log/dnsmasq-ns.openrc.log 2>/dev/null || true"); err == nil {
		dump = append(dump, strings.TrimSpace(string(out)))
	}
	return fmt.Errorf("%s", strings.Join(dump, "\n"))
}

// containsLine is a tiny helper that checks whether `s` contains a line equal
// to `target` (no leading/trailing whitespace). Used so `ip netns list`'s
// trailing " (id: N)" annotations don't break exact match checks.
func containsLine(s, target string) bool {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == target {
			return true
		}
	}
	return false
}
