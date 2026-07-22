package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MintToken requests a signed JWT from the oidc-mock /token endpoint. `base`
// is the issuer URL (e.g. http://127.0.0.1:8080); `sub` and `aud` are claims
// placed in the token; `exp` is the token lifetime added as the `exp` claim.
// The JWT text body is returned verbatim.
func MintToken(ctx context.Context, base, sub, aud string, exp time.Duration) (string, error) {
	form := url.Values{}
	form.Set("sub", sub)
	form.Set("aud", aud)
	form.Set("exp", fmt.Sprintf("%d", int64(exp.Seconds())))

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, strings.TrimRight(base, "/")+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oidc-mock returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return strings.TrimSpace(string(body)), nil
}

// WaitForOidc polls `GET {base}/.well-known/openid-configuration` until it
// returns 200 or `timeout` elapses.
func WaitForOidc(ctx context.Context, base string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := strings.TrimRight(base, "/") + "/.well-known/openid-configuration"
	client := &http.Client{Timeout: 3 * time.Second}
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				_ = resp.Body.Close()
				return nil
			}
			_ = resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errors.New("timeout waiting for oidc-mock")
}
