package state

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	_ "modernc.org/sqlite"
)

type Store struct {
	db     *sql.DB
	logger *slog.Logger
	mu     sync.Mutex
}

func NewStore(dbPath string, logger *slog.Logger) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS services (
			service_name TEXT PRIMARY KEY,
			data TEXT NOT NULL
		);
	`); err != nil {
		return nil, fmt.Errorf("create table: %w", err)
	}

	return &Store{db: db, logger: logger}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) LoadServices() (map[string]*LBService, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query("SELECT service_name, data FROM services")
	if err != nil {
		return nil, fmt.Errorf("query services: %w", err)
	}
	defer rows.Close()

	services := make(map[string]*LBService)
	for rows.Next() {
		var name string
		var data string
		if err := rows.Scan(&name, &data); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}

		var svc LBService
		if err := json.Unmarshal([]byte(data), &svc); err != nil {
			s.logger.Error("failed to unmarshal service, skipping", "service", name, "error", err)
			continue
		}
		services[name] = &svc
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}

	return services, nil
}

func (s *Store) SaveService(svc *LBService) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(svc)
	if err != nil {
		return fmt.Errorf("marshal service: %w", err)
	}

	_, err = s.db.Exec(`
		INSERT INTO services (service_name, data)
		VALUES (?, ?)
		ON CONFLICT(service_name) DO UPDATE SET data = excluded.data;
	`, svc.ServiceName, string(data))
	if err != nil {
		return fmt.Errorf("upsert service: %w", err)
	}

	return nil
}

func (s *Store) DeleteService(serviceName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM services WHERE service_name = ?", serviceName)
	if err != nil {
		return fmt.Errorf("delete service: %w", err)
	}

	return nil
}
