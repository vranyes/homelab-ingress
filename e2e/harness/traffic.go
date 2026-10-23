package harness

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CollectBackendIDs fires `n` sequential HTTP GET requests to
// `http://<lbAddr>/` (lbAddr = e.g. "10.44.0.42:80"), each with a 5s per-request
// timeout, and returns the pod hostname from each successful response.
// The hostname is extracted from the first line of the body returned by
// traefik/whoami (format: "Hostname: <pod-name>"). Failed requests are
// skipped — the returned slice may be shorter than `n`.
//
// WARNING: prefer CollectBackendIDsFreshConn for round-robin assertions.
// Keep-alive reuses the same TCP connection — and thus the same numgen
// evaluation — across requests, which under-counts distinct backends.
// CollectBackendIDs is retained for the churn smoke checks where only
// reachability (not distribution) matters.
func CollectBackendIDs(ctx context.Context, lbAddr string, n int, timeout time.Duration) ([]string, error) {
	url := "http://" + lbAddr + "/"
	client := &http.Client{Timeout: 5 * time.Second}
	ids := make([]string, 0, n)
	deadline := time.Now().Add(timeout)
	for i := 0; i < n; i++ {
		if time.Now().After(deadline) {
			return ids, fmt.Errorf("collecting backend ids timed out after %s, collected %d/%d", timeout, len(ids), n)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return ids, err
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		if id := parseHostname(body); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// parseHostname extracts the pod hostname from a traefik/whoami response,
// which starts with "Hostname: <pod-name>\n".
func parseHostname(body []byte) string {
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "Hostname: ") {
			return strings.TrimSpace(line[len("Hostname: "):])
		}
	}
	return ""
}

// CollectBackendIDsFreshConn is like CollectBackendIDs but forces every HTTP
// request to use a fresh TCP connection (DisableKeepAlives: true). This is
// critical for testing round-robin numgen rules because keep-alive reuses the
// same connection — and thus the same numgen evaluation — across requests.
//
// Unlike CollectBackendIDs it does not silently drop failures: a request that
// cannot connect, or that answers without a "Hostname:" line, is reported as
// an error carrying the first cause. Callers that expect traffic to flow must
// not see an empty slice with a nil error — that hides the difference between
// "the LB is unreachable" and "the LB answered from the wrong place".
func CollectBackendIDsFreshConn(ctx context.Context, lbAddr string, n int, timeout time.Duration) ([]string, error) {
	url := "http://" + lbAddr + "/"
	transport := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: transport,
	}
	ids := make([]string, 0, n)
	var firstErr error
	failures := 0
	fail := func(format string, args ...any) {
		failures++
		if firstErr == nil {
			firstErr = fmt.Errorf(format, args...)
		}
	}
	deadline := time.Now().Add(timeout)
	for i := 0; i < n; i++ {
		if time.Now().After(deadline) {
			return ids, fmt.Errorf("collecting backend ids from %s timed out after %s: collected %d/%d; first error: %v",
				url, timeout, len(ids), n, firstErr)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return ids, err
		}
		resp, err := client.Do(req)
		if err != nil {
			fail("request %d: %w", i+1, err)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			fail("request %d: read body: %w", i+1, err)
			continue
		}
		id := parseHostname(body)
		if id == "" {
			fail("request %d: HTTP %d from %s without a Hostname line (body: %s)",
				i+1, resp.StatusCode, url, bodySnippet(body))
			continue
		}
		ids = append(ids, id)
	}
	if failures > 0 {
		return ids, fmt.Errorf("%d/%d requests to %s failed; first error: %w", failures, n, url, firstErr)
	}
	return ids, nil
}

// statusMarker separates the response body from curl's exit status in the
// output of CollectBackendIDsFromNS.
const statusMarker = "\n__HL_HTTP_STATUS__"

// curlInNS runs curl inside a network namespace. It is a variable so tests can
// substitute a fake without requiring ip/netns or root.
var curlInNS = func(ctx context.Context, ns string, args ...string) ([]byte, error) {
	full := append([]string{"netns", "exec", ns, "curl"}, args...)
	cmd := exec.CommandContext(ctx, "ip", full...)
	return cmd.CombinedOutput()
}

// CollectBackendIDsFromNS is like CollectBackendIDsFreshConn, but sends every
// request from inside the given network namespace.
//
// This distinction is essential in the e2e VM: the LB IPs sit on macvlan
// interfaces in the host namespace, so a request made by the test process
// itself is delivered locally and its DNAT is performed by kube-proxy in the
// OUTPUT chain. Such traffic never reaches the ingress daemon's prerouting
// rules, so assertions built on it pass even when the daemon's DNAT is broken.
// Requests issued from the VEPA peer namespace arrive over the bridge and do
// traverse those chains.
func CollectBackendIDsFromNS(ctx context.Context, ns, lbAddr string, n int, timeout time.Duration) ([]string, error) {
	return collectBackendIDsFromNS(ctx, ns, "", lbAddr, n, timeout)
}

// CollectBackendIDsFromNSSource is CollectBackendIDsFromNS with the client
// bound to a specific source address (curl --interface). It is used to send
// traffic that the mock geo database maps to a different country.
func CollectBackendIDsFromNSSource(ctx context.Context, ns, sourceIP, lbAddr string, n int, timeout time.Duration) ([]string, error) {
	return collectBackendIDsFromNS(ctx, ns, sourceIP, lbAddr, n, timeout)
}

func collectBackendIDsFromNS(ctx context.Context, ns, sourceIP, lbAddr string, n int, timeout time.Duration) ([]string, error) {
	url := "http://" + lbAddr + "/"
	ids := make([]string, 0, n)
	var firstErr error
	failures := 0
	fail := func(format string, args ...any) {
		failures++
		if firstErr == nil {
			firstErr = fmt.Errorf(format, args...)
		}
	}
	deadline := time.Now().Add(timeout)
	for i := 0; i < n; i++ {
		if time.Now().After(deadline) {
			return ids, fmt.Errorf("collecting backend ids from %s (ns %s) timed out after %s: collected %d/%d; first error: %v",
				url, ns, timeout, len(ids), n, firstErr)
		}
		args := []string{"-sS", "-m", "5", "-w", statusMarker + "%{http_code}"}
		if sourceIP != "" {
			args = append(args, "--interface", sourceIP)
		}
		out, err := curlInNS(ctx, ns, append(args, url)...)
		if err != nil {
			fail("request %d: %w: %s", i+1, err, bodySnippet(out))
			continue
		}
		body, status := splitCurlStatus(out)
		if status != "200" {
			fail("request %d: HTTP %s from %s (body: %s)", i+1, status, url, bodySnippet(body))
			continue
		}
		id := parseHostname(body)
		if id == "" {
			fail("request %d: HTTP %s from %s without a Hostname line (body: %s)",
				i+1, status, url, bodySnippet(body))
			continue
		}
		ids = append(ids, id)
	}
	if failures > 0 {
		return ids, fmt.Errorf("%d/%d requests to %s (ns %s) failed; first error: %w", failures, n, url, ns, firstErr)
	}
	return ids, nil
}

// AttemptRequestsFromNS fires `n` short GETs from the namespace, optionally
// bound to sourceIP, and reports how many completed. A dropped SYN makes curl
// exit non-zero; that is the expected signal for geo-blocked traffic.
func AttemptRequestsFromNS(ctx context.Context, ns, sourceIP, lbAddr string, n int, perRequest time.Duration) (attempted, succeeded int, firstErr error) {
	url := "http://" + lbAddr + "/"
	if perRequest <= 0 {
		perRequest = time.Second
	}
	for i := 0; i < n; i++ {
		args := []string{"-sS", "-o", "/dev/null", "-m", fmt.Sprintf("%g", perRequest.Seconds())}
		if sourceIP != "" {
			args = append(args, "--interface", sourceIP)
		}
		out, err := curlInNS(ctx, ns, append(args, url)...)
		attempted++
		if err == nil {
			succeeded++
			continue
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("request %d: %w: %s", i+1, err, bodySnippet(out))
		}
	}
	return attempted, succeeded, firstErr
}

// AttemptParallelFromNS fires `n` HTTP GETs concurrently from inside `ns`,
// optionally bound to sourceIP, and reports how many succeeded. Each request
// is a separate curl process, so each opens a fresh TCP connection — exactly
// what a per-client new-connection limiter counts. Returns an error only when
// the shell itself fails; individual request failures are reflected in the
// returned count.
func AttemptParallelFromNS(ctx context.Context, ns, sourceIP, lbAddr string, n int, perRequest time.Duration) (int, error) {
	if n <= 0 {
		return 0, nil
	}
	if perRequest <= 0 {
		perRequest = time.Second
	}
	iface := ""
	if sourceIP != "" {
		iface = "--interface " + sourceIP + " "
	}
	script := fmt.Sprintf(`res=$(mktemp)
i=0
while [ $i -lt %d ]; do
  ( curl -sS -o /dev/null -m %s 2>/dev/null %s%s && echo 1 >> "$res" || echo 0 >> "$res" ) &
  i=$((i+1))
done
wait
cat "$res"
rm -f "$res"
`, n, strconv.FormatFloat(perRequest.Seconds(), 'g', -1, 64), iface, "http://"+lbAddr+"/")

	out, err := RunShell(ctx, "ip netns exec "+ns+" sh -c "+shellSingleQuote(script))
	if err != nil {
		return 0, fmt.Errorf("parallel requests from %s: %w: %s", ns, err, bodySnippet(out))
	}
	succeeded := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "1" {
			succeeded++
		}
	}
	return succeeded, nil
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// AddNSAddress idempotently assigns an address inside a namespace.
func AddNSAddress(ctx context.Context, ns, dev, cidr string) error {
	out, err := RunShell(ctx, fmt.Sprintf("ip -n %s addr replace %s dev %s", ns, cidr, dev))
	if err != nil {
		return fmt.Errorf("add %s to %s in %s: %w: %s", cidr, dev, ns, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveNSAddress removes an address from a namespace. A missing address is
// not treated as an error (cleanup must be idempotent).
func RemoveNSAddress(ctx context.Context, ns, dev, cidr string) error {
	out, err := RunShell(ctx, fmt.Sprintf("ip -n %s addr del %s dev %s", ns, cidr, dev))
	if err != nil {
		msg := string(out)
		if strings.Contains(msg, "Cannot assign requested address") || strings.Contains(msg, "RTNETLINK answers: No such") {
			return nil
		}
		return fmt.Errorf("remove %s from %s in %s: %w: %s", cidr, dev, ns, err, strings.TrimSpace(msg))
	}
	return nil
}

// splitCurlStatus splits curl's -w output into the response body and the HTTP
// status code.
func splitCurlStatus(out []byte) (body []byte, status string) {
	idx := strings.LastIndex(string(out), strings.TrimSpace(statusMarker))
	if idx < 0 {
		return out, "unknown"
	}
	statusStart := idx + len(strings.TrimSpace(statusMarker))
	return out[:idx], strings.TrimSpace(string(out[statusStart:]))
}

// bodySnippet trims a response body to a short single-line form for error
// messages.
func bodySnippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	s = strings.ReplaceAll(s, "\n", `\n`)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	if s == "" {
		return "<empty>"
	}
	return s
}

// DistinctIDs returns the sorted set of unique strings in `ids`.
func DistinctIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Distribution returns a map of backend id → count over `ids`.
func Distribution(ids []string) map[string]int {
	dist := make(map[string]int, len(ids))
	for _, id := range ids {
		dist[id]++
	}
	return dist
}

// AssertAllHit returns nil iff the number of distinct backend ids in `ids`
// equals `expected`. It does not care about distribution shape — just equality
// of distinct count.
func AssertAllHit(ids []string, expected int) error {
	d := DistinctIDs(ids)
	if len(d) != expected {
		return fmt.Errorf("expected %d distinct backend ids, got %d: %v", expected, len(d), d)
	}
	return nil
}

// AssertRoughlyUniform returns nil iff the distribution of distinct ids in
// `ids` is within `tolerance` (a fraction) of uniform. Specifically, each
// distinct id must appear in `[(1-tol)/D * N, (1+tol)/D * N]` occurrences
// where N=len(ids) and D=#distinct. Required: ids is non-empty and has at
// least 2 distinct backends.
func AssertRoughlyUniform(ids []string, tolerance float64) error {
	if len(ids) == 0 {
		return fmt.Errorf("expected at least one response to assert distribution against")
	}
	dist := Distribution(ids)
	d := len(dist)
	if d < 2 {
		return fmt.Errorf("expected ≥2 distinct backend ids for uniformity check, got %d", d)
	}
	ideal := float64(len(ids)) / float64(d)
	low := ideal * (1 - tolerance)
	high := ideal * (1 + tolerance)
	for id, c := range dist {
		f := float64(c)
		if f < low || f > high {
			return fmt.Errorf("distribution uneven: backend %s got %d hits, want within [%.1f, %.1f] (uniform=%.1f)", id, c, low, high, ideal)
		}
	}
	return nil
}
