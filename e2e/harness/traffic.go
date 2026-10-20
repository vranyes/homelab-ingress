package harness

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
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
func CollectBackendIDsFreshConn(ctx context.Context, lbAddr string, n int, timeout time.Duration) ([]string, error) {
	url := "http://" + lbAddr + "/"
	transport := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: transport,
	}
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
