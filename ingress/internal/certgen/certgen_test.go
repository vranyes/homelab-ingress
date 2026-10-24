package certgen

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadOrCreate_ReusesExistingValidCA pins the restart contract: a
// second LoadOrCreate against the same state dir with a valid CA on disk
// must return the identical CA and leave both files untouched. This is
// the baseline the fix must preserve.
func TestLoadOrCreate_ReusesExistingValidCA(t *testing.T) {
	dir := t.TempDir()

	_, caPEM1, err := LoadOrCreate(dir, []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}
	certBefore, err := os.ReadFile(caCertPath(dir))
	if err != nil {
		t.Fatalf("read ca.pem: %v", err)
	}
	keyBefore, err := os.ReadFile(caKeyPath(dir))
	if err != nil {
		t.Fatalf("read ca-key.pem: %v", err)
	}

	// Second call simulates a daemon restart against the same state dir.
	_, caPEM2, err := LoadOrCreate(dir, []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if !bytes.Equal(caPEM1, caPEM2) {
		t.Fatalf("CA was regenerated across restarts despite a valid CA on disk")
	}
	if certAfter, _ := os.ReadFile(caCertPath(dir)); !bytes.Equal(certBefore, certAfter) {
		t.Fatalf("ca.pem was rewritten despite a valid CA on disk")
	}
	if keyAfter, _ := os.ReadFile(caKeyPath(dir)); !bytes.Equal(keyBefore, keyAfter) {
		t.Fatalf("ca-key.pem was rewritten despite a valid CA on disk")
	}
}

// TestLoadOrCreate_MissingKeyDoesNotClobberValidCert demonstrates the
// reported live bug: ca.pem exists and is valid but ca-key.pem is missing
// (deleted, never persisted, or unreadable by the service user). The
// current fall-through in loadOrCreateCA silently mints a brand-new CA and
// overwrites the valid ca.pem instead of returning an error — so every
// restart rotates the CA out from under clients pinning ca.pem.
func TestLoadOrCreate_MissingKeyDoesNotClobberValidCert(t *testing.T) {
	dir := t.TempDir()

	if _, _, err := LoadOrCreate(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatalf("seed LoadOrCreate: %v", err)
	}
	certBefore, err := os.ReadFile(caCertPath(dir))
	if err != nil {
		t.Fatalf("read ca.pem: %v", err)
	}

	// Simulate the live state: valid cert on disk, key gone.
	if err := os.Remove(caKeyPath(dir)); err != nil {
		t.Fatalf("remove ca-key.pem: %v", err)
	}

	_, _, err = LoadOrCreate(dir, []string{"127.0.0.1"})
	if err == nil {
		t.Fatalf("BUG: expected an error when ca-key.pem is missing with a valid ca.pem present; " +
			"LoadOrCreate succeeded, i.e. it regenerated the CA and clobbered the valid ca.pem")
	}

	if certAfter, _ := os.ReadFile(caCertPath(dir)); !bytes.Equal(certBefore, certAfter) {
		t.Fatalf("BUG: ca.pem was overwritten even though the call failed; valid cert was clobbered")
	}
}

// TestLoadOrCreate_RefusesNewCAWhenStateDBExists pins the guard against silent
// rotation after a partial restore: an initialised state directory whose CA
// material is gone must fail loudly, not mint a new trust anchor.
func TestLoadOrCreate_RefusesNewCAWhenStateDBExists(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stateDBFile), []byte("db"), 0644); err != nil {
		t.Fatalf("seed state.db: %v", err)
	}

	_, _, err := LoadOrCreate(dir, []string{"127.0.0.1"})
	if err == nil {
		t.Fatal("expected an error when state.db exists without CA material")
	}
	if fileExists(caCertPath(dir)) || fileExists(caKeyPath(dir)) {
		t.Fatal("CA material was written despite the refusal")
	}
}

// TestLoadOrCreate_GeneratesForFreshDir keeps the legitimate first-boot path:
// no state.db, no CA files.
func TestLoadOrCreate_GeneratesForFreshDir(t *testing.T) {
	dir := t.TempDir()
	_, caPEM, err := LoadOrCreate(dir, []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if len(caPEM) == 0 {
		t.Fatal("expected a CA PEM")
	}
	if !fileExists(caCertPath(dir)) || !fileExists(caKeyPath(dir)) {
		t.Fatal("CA material was not persisted")
	}
}

// TestLoadOrCreate_CorruptCertReturnsError locks in the existing (good)
// behavior for the mirror case: a corrupt ca.pem with a valid key present
// surfaces an error instead of silently regenerating. Together with the
// test above this pins down the asymmetry: corrupt cert → error, but
// missing key → silent regen.
func TestLoadOrCreate_CorruptCertReturnsError(t *testing.T) {
	dir := t.TempDir()

	if _, _, err := LoadOrCreate(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatalf("seed LoadOrCreate: %v", err)
	}
	keyBefore, err := os.ReadFile(caKeyPath(dir))
	if err != nil {
		t.Fatalf("read ca-key.pem: %v", err)
	}

	if err := os.WriteFile(caCertPath(dir), []byte("not a pem file"), 0644); err != nil {
		t.Fatalf("corrupt ca.pem: %v", err)
	}

	if _, _, err := LoadOrCreate(dir, []string{"127.0.0.1"}); err == nil {
		t.Fatalf("expected an error for a corrupt ca.pem, got nil")
	}

	if keyAfter, _ := os.ReadFile(caKeyPath(dir)); !bytes.Equal(keyBefore, keyAfter) {
		t.Fatalf("ca-key.pem was rewritten on the corrupt-cert error path")
	}
}
