package state

import (
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func testStoreLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestLoadServicesSkipsCorruptRow pins the corrupt-row path: a service row
// whose JSON does not decode is skipped (with a log line) while valid rows
// still load, so one bad write can never wedge daemon startup.
func TestLoadServicesSkipsCorruptRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	store, err := NewStore(dbPath, testStoreLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.SaveService(&LBService{ServiceName: "ns/good"}); err != nil {
		t.Fatalf("SaveService: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Inject a corrupt row behind the Store's back.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO services (service_name, data) VALUES (?, ?)`,
		"ns/bad", "{bad json"); err != nil {
		_ = raw.Close()
		t.Fatalf("insert corrupt row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	reopened, err := NewStore(dbPath, testStoreLogger())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	services, err := reopened.LoadServices()
	if err != nil {
		t.Fatalf("LoadServices must skip corrupt rows, not fail: %v", err)
	}
	if _, ok := services["ns/good"]; !ok {
		t.Fatalf("valid service must still load")
	}
	if _, ok := services["ns/bad"]; ok {
		t.Fatalf("corrupt service must be skipped")
	}
}
