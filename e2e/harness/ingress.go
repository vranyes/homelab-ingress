package harness

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	routerv1 "github.com/vranyes/homelab-ingress/controller/pkg/api/router/v1"
	"gopkg.in/yaml.v3"
)

// IngressConfig mirrors the YAML shape consumed by the ingress daemon
// (see ingress/internal/config/config.go). It exists purely to give the
// harness a type-safe way to construct a config file from scratch.
type IngressConfig struct {
	BaseInterface string           `yaml:"base_interface"`
	GRPC          IngressGRPC       `yaml:"grpc"`
	WS            IngressWS         `yaml:"ws"`
	Auth          IngressAuth       `yaml:"auth"`
	Masquerade    []MasqueradeRule  `yaml:"masquerade,omitempty"`
}

// IngressGRPC is the listen address for the gRPC server.
type IngressGRPC struct {
	Listen string `yaml:"listen"`
}

// IngressWS is the listen address for the WebSocket event stream.
// Empty disables the WS server.
type IngressWS struct {
	Listen string `yaml:"listen"`
}

// IngressAuth mirrors the auth block of the ingress config.
type IngressAuth struct {
	Issuer          string   `yaml:"issuer"`
	Audience        string   `yaml:"audience"`
	AllowedSubjects []string `yaml:"allowed_subjects"`
	TrustedRoot     string   `yaml:"trusted_root_ca,omitempty"`
}

// MasqueradeRule mirrors ingress MasqueradeRule.
type MasqueradeRule struct {
	InputDevice  string `yaml:"input_device"`
	OutputDevice string `yaml:"output_device"`
}

// DefaultIngressConfig returns the config the e2e suite installs at
// /etc/homelab-ingress/config.yaml. The OIDC issuer, audience, and allowed
// subject match the values minted by harness.MintToken inside the suite's
// BeforeSuite.
func DefaultIngressConfig() IngressConfig {
	return IngressConfig{
		BaseInterface: "hl-br0",
		GRPC:          IngressGRPC{Listen: "127.0.0.1:50051"},
		WS:            IngressWS{Listen: ""},
		Auth: IngressAuth{
			Issuer:          "http://127.0.0.1:8080",
			Audience:        "homelab-ingress",
			AllowedSubjects: []string{"system:serviceaccount:default:homelab-ingress-controller"},
		},
	}
}

// WriteIngressConfig marshals `cfg` to YAML and writes it to `path`
// with mode 0644, creating parent directories as needed.
func WriteIngressConfig(path string, cfg IngressConfig) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal ingress config: %w", err)
	}
	return WriteFile(path, data, 0o644)
}

// tokenFileCredentials mirrors controller/internal/router.tokenFileCredentials:
// it reads a Bearer token from a file on every RPC so rotated tokens take
// effect without re-dialing.
type tokenFileCredentials struct {
	tokenPath string
}

func (t *tokenFileCredentials) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	data, err := os.ReadFile(t.tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read token file %q: %w", t.tokenPath, err)
	}
	return map[string]string{
		"authorization": "Bearer " + strings.TrimSpace(string(data)),
	}, nil
}

func (t *tokenFileCredentials) RequireTransportSecurity() bool { return true }

// NewRouterClient builds a gRPC client targeting the ingress RouterService
// at `addr`, with TLS rooted in `caPEMPath` and a Bearer token taken on each
// RPC from `tokenPath`. This mirrors controller/internal/router.NewClient so
// the e2e harness sees exactly the same connection characteristics.
func NewRouterClient(addr, tokenPath, caPath string) (routerv1.RouterServiceClient, *grpc.ClientConn, error) {
	caData, err := os.ReadFile(caPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA cert %q: %w", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caData) {
		return nil, nil, fmt.Errorf("no valid CA cert in %q", caPath)
	}
	tlsCfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithPerRPCCredentials(&tokenFileCredentials{tokenPath: tokenPath}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dial router service: %w", err)
	}
	return routerv1.NewRouterServiceClient(conn), conn, nil
}

// WaitForIngress polls the router `Health` RPC until it succeeds within
// `timeout`. It also waits for the CA PEM at `caPEMPath` to exist.
func WaitForIngress(ctx context.Context, addr, caPEMPath, tokenPath string, timeout time.Duration) error {
	if err := WaitForFile(ctx, caPEMPath, 30*time.Second); err != nil {
		return fmt.Errorf("wait for CA pem: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cl, conn, err := NewRouterClient(addr, tokenPath, caPEMPath)
		if err == nil {
			reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_, err = cl.Health(reqCtx, &routerv1.HealthRequest{})
			cancel()
			_ = conn.Close()
			if err == nil {
				return nil
			}
			if status.Code(err) != codes.Unavailable {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("timeout waiting for ingress to become healthy after %s", timeout)
}
