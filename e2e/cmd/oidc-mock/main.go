// Command oidc-mock is a minimal mock OIDC provider used by the homelab-ingress
// end-to-end test harness. It exposes a discovery document, a JWKS endpoint, and
// a token-minting endpoint that returns a signed RS256 JWT as a text/plain body.
//
// The RSA signing key is embedded deterministically as a PEM block so the JWKS
// validates across process restarts.
package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// embeddedKeyPEM is a fixed RSA-2048 PKCS1 private key used to sign minted
// tokens. Embedding it (rather than generating at startup) keeps the JWKS
// stable across restarts so the ingress validator can trust the kid.
const embeddedKeyPEM = `-----BEGIN RSA PRIVATE KEY-----
MIIEogIBAAKCAQEAkO02D8bbexRwBN+q7N3yZ8NlOUHqaMEsk57t693yeTFhOOSK
HryIxIHnIXLvUWn8tQpYmfvpQUAVPjEHUwhPNu47T2uB9TE+GCrTz3C8vT3rwqwR
kNviZ9Hy713K+QFGRq5z94uQCjPgVBF6G3YceMCoXxtS0sEC8/nh1c+9e+Wvngw4
y/pbNwNLS1geQtMxVo6oic1pkDWgk9tZrxiEJWIuaqE0a2g0dqYa8LrTQwB4IPy7
mDY1uyIx+UnHlHSWAAVFDTyHY2nRCFdHT2+gfh6NAw4z+bBOAZEDDP1NF+3AsBUm
mUaDOsWzyI6B/d3Jw+r0kG7+8oA6buUjZt0gFwIDAQABAoIBADb+VE/bSSikojcv
OnhkZhn6urvl59G1T7l6itTAZUSYuMgybLjqGMncczytlONjYPm/wcw4m5PfgdAW
Ia3fztNMxiPGa0uD0oiwI6ypHm1/BCncSlQaJAj6UYGEKUOguK6UfzvVcSR19+9e
rGOkR3yIurtQLnb7t6el0AzmpgPPm8YL3CpYYWFAFEuk5VjzQBiLN3ZYrCOzy8Rw
sJxqxifTIDf4VjMaiLhDjYXSXXh3uzwMu6ovGpWmJrski5l4x21s9h6qMXWt2Vt9
uKqjqGg49XUS6Yl7I8+I6OjPSkSIp3llwuCoQmOP+TX249xpat9S+EvREDzITBId
2ocfs30CgYEAxCdOn06eId/a3IvpTsdaAfEPqxWq/0Zy5jewmNnzd7h52yB0o3Xl
GjKGuhr446MSyDITmHKU/CI/85Zkf/Z9VUu9laZZkbre98SeNbpu9hqvqQA/l5W2
RuaOgOKBcYxksoI9Z8r+iXGZVBDkGUlmmbdIhs2G3Pgg9+0bPT2X8cMCgYEAvSTK
7aW6cFiMTlL2IdzheDLkV0zhp6R9ISpNdu9ZgwyL16AKyhZGpuwZg0Z9a5qf+YH9
GG5/0VXhwtfdfA4lxeXoxHum2x8SN2K1HodDACZyZizLCFS2ug1Q+eZqt5BNZ+b+
+oiELOiNItVWZnfPQZ/Gh3KMBSfvA5B2uIr9fx0CgYAwGEcxzm/9dErQismg1EZg
dX/W/hAYWWiww/Tb9Yy5zRSrwtB2h+CtxLe+CAEAp6piRc0CMw4OqEMv8a8gpQpk
EJcii1Ao6D3lWGNGXzj3xOFPOc1d2IqB5eQI/ACrG7ZRApgVy/MPNjhXIXEcNiAf
Do6M5vXrVjeXMp7cLC8GSwKBgBqgdVjjPc3XCVl1YGT//VW4hGCt2A6Ln3ntxJ37
r/Q87erbAunUHPoevrC2Wa2uRNsRK5om5COKHZAaZ3mxXCvx48TVS3YJd70ITksP
66S3xN8RazuvAVJL0Zx8jYVt8Rexcy+9zC2uR0AiHFmsgCbS4VXsED4hkwpDahMs
recZAoGAfvWVllENBEoDOpWZ2RExS7aBDu9gTSoVR6YTzOvXOOjiWMXhy21Ypkrd
DFGBkXyIJ1jq0wMKklTHaj2nlw6kjFs3pCozbCmWTxqzEQHeSnlkd0fEb2q6uj2P
8IVBadTjo3zYBREsg278qyTNcwfydCentaqnjKzJqTxsx2VdWn8=
-----END RSA PRIVATE KEY-----`

// keyID is the stable kid placed in the JWT header and the JWKS entry.
const keyID = "hl-ingress-test-key-1"

// defaultListen is the default bind address.
const defaultListen = "127.0.0.1:8080"

// defaultExpSeconds is the default token lifetime (24h) when exp is omitted.
const defaultExpSeconds int64 = 86400

// oidcProvider holds the parsed signing key and derived public material.
type oidcProvider struct {
	privateKey *rsa.PrivateKey
	issuer     string
	jwks       joseJSONWebKeySet
}

// joseJSONWebKeySet is a minimal JWKS document per RFC 7517.
type joseJSONWebKeySet struct {
	Keys []joseJSONWebKey `json:"keys"`
}

// joseJSONWebKey is a single JWK entry. Only the fields needed by the ingress
// validator are populated.
type joseJSONWebKey struct {
	KTY string   `json:"kty"`
	Use string   `json:"use"`
	KID string   `json:"kid"`
	Alg string   `json:"alg"`
	N   string   `json:"n"`
	E   string   `json:"e"`
}

// discoveryDocument is the subset of the OIDC discovery payload we serve.
type discoveryDocument struct {
	Issuer                           string   `json:"issuer"`
	JwksURI                          string   `json:"jwks_uri"`
	TokenEndpoint                    string   `json:"token_endpoint"`
	IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`
}

// tokenClaims is the set of JWT claims minted by the /token endpoint.
type tokenClaims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Audience  string `json:"aud"`
	IssuedAt  int64  `json:"iat"`
	NotBefore int64  `json:"nbf"`
	ExpiresAt int64  `json:"exp"`
}

// jwtHeader is the JOSE header placed on minted tokens.
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	KID string `json:"kid"`
}

// apiError is the JSON error body returned on validation failures.
type apiError struct {
	Error string `json:"error"`
}

func main() {
	listen := flag.String("listen", defaultListen, "listen address (host:port or :port)")
	flag.Parse()

	key, err := loadSigningKey()
	if err != nil {
		log.Fatalf("oidc-mock: load signing key: %v", err)
	}

	issuer, err := deriveIssuer(*listen)
	if err != nil {
		log.Fatalf("oidc-mock: derive issuer from %q: %v", *listen, err)
	}

	provider := &oidcProvider{
		privateKey: key,
		issuer:     issuer,
		jwks: joseJSONWebKeySet{
			Keys: []joseJSONWebKey{publicJWK(&key.PublicKey, keyID)},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", provider.handleDiscovery)
	mux.HandleFunc("GET /jwks", provider.handleJWKS)
	mux.HandleFunc("GET /token", provider.handleToken)
	mux.HandleFunc("POST /token", provider.handleToken)

	srv := &http.Server{
		Addr:     *listen,
		Handler:  loggingMiddleware(mux),
		ErrorLog: log.New(os.Stderr, "oidc-mock: ", log.LstdFlags),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("oidc-mock: listening on %s (issuer=%s)", *listen, issuer)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("oidc-mock: server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("oidc-mock: shutdown signal received, draining...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("oidc-mock: graceful shutdown failed: %v", err)
	}
}

// loadSigningKey parses the embedded PKCS1 RSA PEM. If the embedded key is
// somehow malformed it falls back to generating an ephemeral key (with a loud
// warning) so the server still boots in degraded environments.
func loadSigningKey() (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(embeddedKeyPEM))
	if block == nil {
		log.Printf("oidc-mock: WARNING: embedded key PEM failed to decode; generating ephemeral key (JWKS will not be stable across restarts)")
		return rsa.GenerateKey(rand.Reader, 2048)
	}
	if block.Type == "RSA PRIVATE KEY" {
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			log.Printf("oidc-mock: WARNING: ParsePKCS1PrivateKey failed: %v; generating ephemeral key", err)
			return rsa.GenerateKey(rand.Reader, 2048)
		}
		return key, nil
	}
	// Tolerate PKCS8 wrappers as a fallback.
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
	}
	log.Printf("oidc-mock: WARNING: embedded key is not PKCS1 RSA; generating ephemeral key")
	return rsa.GenerateKey(rand.Reader, 2048)
}

// deriveIssuer converts a listen address into an http:// URL. An empty host
// (":8080") defaults to 127.0.0.1.
func deriveIssuer(listen string) (string, error) {
	if listen == "" {
		return "", errors.New("empty listen address")
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("split host:port: %w", err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

// publicJWK builds a JWK from an RSA public key with the given kid.
func publicJWK(pub *rsa.PublicKey, kid string) joseJSONWebKey {
	return joseJSONWebKey{
		KTY: "RSA",
		Use: "sig",
		KID: kid,
		Alg: "RS256",
		N:   base64urlEncode(pub.N.Bytes()),
		E:   base64urlEncode(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func (p *oidcProvider) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	doc := discoveryDocument{
		Issuer:                           p.issuer,
		JwksURI:                          p.issuer + "/jwks",
		TokenEndpoint:                    p.issuer + "/token",
		IDTokenSigningAlgValuesSupported: []string{"RS256"},
	}
	writeJSON(w, http.StatusOK, doc)
}

func (p *oidcProvider) handleJWKS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.jwks)
}

func (p *oidcProvider) handleToken(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			writeError(w, http.StatusBadRequest, "malformed form body")
			return
		}
		values = r.Form
	}

	sub := strings.TrimSpace(values.Get("sub"))
	aud := strings.TrimSpace(values.Get("aud"))
	if sub == "" || aud == "" {
		writeError(w, http.StatusBadRequest, "sub and aud are required")
		return
	}

	expSeconds := defaultExpSeconds
	if raw := strings.TrimSpace(values.Get("exp")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "exp must be a positive integer (seconds)")
			return
		}
		expSeconds = n
	}

	now := time.Now().Unix()
	claims := tokenClaims{
		Issuer:    p.issuer,
		Subject:   sub,
		Audience:  aud,
		IssuedAt:  now,
		NotBefore: now,
		ExpiresAt: now + expSeconds,
	}

	token, err := p.sign(claims)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("sign token: %v", err))
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(token))
}

// sign produces a compact JWS (header.payload.signature) signed with RS256.
func (p *oidcProvider) sign(claims tokenClaims) (string, error) {
	header := jwtHeader{Alg: "RS256", Typ: "JWT", KID: keyID}
	hBytes, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("marshal header: %w", err)
	}
	cBytes, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}

	signingInput := base64urlEncode(hBytes) + "." + base64urlEncode(cBytes)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("rsa sign: %w", err)
	}
	return signingInput + "." + base64urlEncode(sig), nil
}

// writeJSON serialises v as JSON and sends it with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError sends a JSON error body.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg})
}

// loggingMiddleware emits one line per request to stderr.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("ts=%d method=%s path=%s status=%d", time.Now().Unix(), r.Method, r.URL.Path, rec.status)
	})
}

// statusRecorder captures the status code written by the inner handler.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// base64urlEncode returns the raw bytes as unpadded base64url (RFC 7515).
func base64urlEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
