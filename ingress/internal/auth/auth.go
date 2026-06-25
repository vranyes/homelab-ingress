package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"strings"
	"time"

	"log/slog"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type subjectContextKey struct{}

// wrappedStream lets us replace the Context() on an incoming ServerStream
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

// SubjectFromContext returns the authenticated subject if present.
func SubjectFromContext(ctx context.Context) (string, bool) {
	v := ctx.Value(subjectContextKey{})
	s, ok := v.(string)
	return s, ok
}

// NewInterceptors creates unary and stream gRPC interceptors that validate
// incoming requests carry a Bearer token signed by the given OIDC issuer and
// that the `sub` claim is present in the allowedSubjects list.
// When audience is non-empty, the token's aud claim must contain it.
// trustedRoot is an optional cert pool to use when connecting to the OIDC
// issuer; if nil, the system pool is used.
func NewInterceptors(ctx context.Context, issuer, audience string, allowedSubjects []string, trustedRoot *x509.CertPool, logger *slog.Logger) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor, error) {
	if trustedRoot != nil {
		hc := &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: trustedRoot},
			},
		}
		ctx = context.WithValue(ctx, oauth2.HTTPClient, hc)
	}

	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, nil, err
	}

	verifierCfg := &oidc.Config{}
	if audience != "" {
		verifierCfg.ClientID = audience
	} else {
		verifierCfg.SkipClientIDCheck = true
	}
	verifier := provider.Verifier(verifierCfg)

	allowed := make(map[string]struct{}, len(allowedSubjects))
	for _, s := range allowedSubjects {
		allowed[s] = struct{}{}
	}

	verifyAndInject := func(ctx context.Context) (context.Context, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing metadata")
		}

		vals := md.Get("authorization")
		if len(vals) == 0 {
			vals = md.Get("Authorization")
		}
		if len(vals) == 0 {
			return nil, status.Error(codes.Unauthenticated, "authorization header missing")
		}

		var raw string
		for _, v := range vals {
			if strings.HasPrefix(strings.ToLower(v), "bearer ") {
				raw = v[len("Bearer "):]
				break
			}
		}
		if raw == "" {
			return nil, status.Error(codes.Unauthenticated, "bearer token missing")
		}

		// Verify token with a short timeout
		vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		idTok, err := verifier.Verify(vctx, raw)
		if err != nil {
			logger.Debug("token verify failed", "error", err)
			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}

		var claims struct {
			Subject string `json:"sub"`
		}
		if err := idTok.Claims(&claims); err != nil {
			logger.Debug("reading token claims failed", "error", err)
			return nil, status.Error(codes.Unauthenticated, "invalid token claims")
		}

		if _, ok := allowed[claims.Subject]; !ok {
			logger.Info("unauthorized subject", "sub", claims.Subject)
			return nil, status.Error(codes.PermissionDenied, "subject not authorized")
		}

		return context.WithValue(ctx, subjectContextKey{}, claims.Subject), nil
	}

	unary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		nctx, err := verifyAndInject(ctx)
		if err != nil {
			return nil, err
		}
		return handler(nctx, req)
	}

	stream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		nctx, err := verifyAndInject(ss.Context())
		if err != nil {
			return err
		}
		ws := &wrappedStream{ServerStream: ss, ctx: nctx}
		return handler(srv, ws)
	}

	return unary, stream, nil
}
