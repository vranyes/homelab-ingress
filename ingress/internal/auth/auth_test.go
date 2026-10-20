package auth

import (
	"context"
	"net/http"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func codeOf(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	s, ok := status.FromError(err)
	if !ok {
		return codes.Unknown
	}
	return s.Code()
}

func TestExtractBearerToken(t *testing.T) {
	newCtx := func(kv ...string) (ctx context.Context) {
		return metadata.NewIncomingContext(context.Background(), metadata.Pairs(kv...))
	}

	t.Run("lowercase header", func(t *testing.T) {
		got, err := ExtractBearerToken(newCtx("authorization", "Bearer abc123"))
		if err != nil || got != "abc123" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("capitalized header", func(t *testing.T) {
		got, err := ExtractBearerToken(newCtx("Authorization", "Bearer abc123"))
		if err != nil || got != "abc123" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("scheme case-insensitive", func(t *testing.T) {
		got, err := ExtractBearerToken(newCtx("authorization", "bearer abc123"))
		if err != nil || got != "abc123" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("missing metadata", func(t *testing.T) {
		if _, err := ExtractBearerToken(context.Background()); codeOf(err) != codes.Unauthenticated {
			t.Fatalf("expected Unauthenticated, got %v", err)
		}
	})

	t.Run("missing header", func(t *testing.T) {
		if _, err := ExtractBearerToken(newCtx("other", "x")); codeOf(err) != codes.Unauthenticated {
			t.Fatalf("expected Unauthenticated, got %v", err)
		}
	})

	t.Run("non-bearer scheme rejected", func(t *testing.T) {
		if _, err := ExtractBearerToken(newCtx("authorization", "Basic abc123")); codeOf(err) != codes.Unauthenticated {
			t.Fatalf("expected Unauthenticated, got %v", err)
		}
	})
}

func TestExtractBearerTokenHTTP(t *testing.T) {
	newReq := func(value string) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, "http://localhost/", nil)
		if value != "" {
			r.Header.Set("Authorization", value)
		}
		return r
	}

	t.Run("valid", func(t *testing.T) {
		got, err := ExtractBearerTokenHTTP(newReq("Bearer abc123"))
		if err != nil || got != "abc123" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		if _, err := ExtractBearerTokenHTTP(newReq("")); codeOf(err) != codes.Unauthenticated {
			t.Fatalf("expected Unauthenticated, got %v", err)
		}
	})

	t.Run("non-bearer rejected", func(t *testing.T) {
		if _, err := ExtractBearerTokenHTTP(newReq("Basic abc123")); codeOf(err) != codes.Unauthenticated {
			t.Fatalf("expected Unauthenticated, got %v", err)
		}
	})
}
