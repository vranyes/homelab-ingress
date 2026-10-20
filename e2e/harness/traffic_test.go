package harness

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCollectBackendIDsFreshConnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Hostname: pod-abc\nIP: 10.42.0.9\n"))
	}))
	defer srv.Close()

	ids, err := CollectBackendIDsFreshConn(context.Background(), strings.TrimPrefix(srv.URL, "http://"), 5, 10*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 5 {
		t.Fatalf("got %d ids, want 5", len(ids))
	}
	for _, id := range ids {
		if id != "pod-abc" {
			t.Fatalf("id = %q, want pod-abc", id)
		}
	}
}

// TestCollectBackendIDsFreshConnReportsUnreachable pins that connection
// failures surface as an error instead of an empty, nil-error slice (which is
// how a broken LB previously looked like a test-harness mystery).
func TestCollectBackendIDsFreshConnReportsUnreachable(t *testing.T) {
	// Reserve a port and close it so nothing is listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ids, err := CollectBackendIDsFreshConn(context.Background(), addr, 3, 3*time.Second)
	if err == nil {
		t.Fatalf("expected an error for an unreachable LB, got ids=%v", ids)
	}
	if len(ids) != 0 {
		t.Fatalf("expected no ids, got %v", ids)
	}
	if !strings.Contains(err.Error(), "3/3 requests") {
		t.Fatalf("error must report the failure count, got: %v", err)
	}
}

// TestCollectBackendIDsFreshConnReportsNonWhoamiBody pins the other silent
// path: a request that succeeds at the HTTP level but does not come from a
// whoami backend (wrong DNAT target, error page) must be an error, not a
// skipped result.
func TestCollectBackendIDsFreshConnReportsNonWhoamiBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no healthy upstream", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ids, err := CollectBackendIDsFreshConn(context.Background(), strings.TrimPrefix(srv.URL, "http://"), 2, 3*time.Second)
	if err == nil {
		t.Fatalf("expected an error, got ids=%v", ids)
	}
	msg := err.Error()
	if !strings.Contains(msg, "503") || !strings.Contains(msg, "no healthy upstream") {
		t.Fatalf("error must include status and body, got: %v", msg)
	}
}

func withFakeCurl(t *testing.T, fn func(context.Context, string, ...string) ([]byte, error)) {
	t.Helper()
	old := curlInNS
	curlInNS = fn
	t.Cleanup(func() { curlInNS = old })
}

func TestCollectBackendIDsFromNSParsesStatusMarker(t *testing.T) {
	withFakeCurl(t, func(_ context.Context, ns string, args ...string) ([]byte, error) {
		if ns != "ns-server" {
			t.Fatalf("ns = %q, want ns-server", ns)
		}
		if got := args[len(args)-1]; got != "http://10.44.0.9:80/" {
			t.Fatalf("url = %q", got)
		}
		return []byte("Hostname: pod-ns\nIP: 10.44.0.2\n" + statusMarker + "200"), nil
	})

	ids, err := CollectBackendIDsFromNS(context.Background(), "ns-server", "10.44.0.9:80", 3, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("got %d ids, want 3", len(ids))
	}
	if ids[0] != "pod-ns" {
		t.Fatalf("id = %q, want pod-ns", ids[0])
	}
}

func TestCollectBackendIDsFromNSReportsCurlFailure(t *testing.T) {
	withFakeCurl(t, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("curl: (7) Failed to connect"), errors.New("exit status 7")
	})

	ids, err := CollectBackendIDsFromNS(context.Background(), "ns-server", "10.44.0.9:80", 2, 3*time.Second)
	if err == nil {
		t.Fatalf("expected an error, got ids=%v", ids)
	}
	if !strings.Contains(err.Error(), "2/2 requests") || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("error must report count and cause, got: %v", err)
	}
}

func TestCollectBackendIDsFromNSReportsNon200(t *testing.T) {
	withFakeCurl(t, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("no healthy upstream" + statusMarker + "503"), nil
	})

	ids, err := CollectBackendIDsFromNS(context.Background(), "ns-server", "10.44.0.9:80", 1, 3*time.Second)
	if err == nil {
		t.Fatalf("expected an error, got ids=%v", ids)
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("error must name the status, got: %v", err)
	}
}
