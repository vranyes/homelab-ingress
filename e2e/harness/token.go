package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WriteToken writes a JWT to `jwtPath` with mode 0600, creating parent
// directories as needed.
func WriteToken(jwtPath, jwt string) error {
	if err := os.MkdirAll(filepath.Dir(jwtPath), 0o700); err != nil {
		return fmt.Errorf("mkdir parent of %s: %w", jwtPath, err)
	}
	return os.WriteFile(jwtPath, []byte(jwt), 0o600)
}

// MutateToken atomically replaces the contents of `jwtPath` with `newJWT`
// by writing to a sibling temp file and renaming over the target.
func MutateToken(jwtPath, newJWT string) error {
	dir := filepath.Dir(jwtPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir parent of %s: %w", jwtPath, err)
	}
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return fmt.Errorf("create temp token file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(newJWT); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp token: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod temp token: %w", err)
	}
	return os.Rename(tmpName, jwtPath)
}

// BadAudienceToken mints a token with aud="bogus" so the ingress service
// rejects it with Unauthenticated.
func BadAudienceToken(ctx context.Context, oidcBase, sub string) (string, error) {
	return MintToken(ctx, oidcBase, sub, "bogus", 1*time.Hour)
}
