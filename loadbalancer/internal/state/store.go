package state

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// mu serializes access to the in-memory State objects that are shared
// across goroutines via the package-level helper functions. Callers that
// modify the state directly must take care to coordinate with this mutex.
var mu sync.RWMutex

type Store struct {
	path string
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

func (s *Store) Load() (*State, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}

	if st.Services == nil {
		st.Services = make(map[string]*LBService)
	}

	return &st, nil
}

// Save writes state to disk atomically. It marshals the state to a temp file
// in the same directory and then renames it into place. The write is
// protected by a package-level mutex to avoid concurrent Save / Update races.
func (s *Store) Save(st *State) error {
	mu.Lock()
	defer mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	// Best effort: ignore sync/flush and rely on rename for atomicity.
	return os.Rename(tmp, s.path)
}

// Package-level helpers that mutate or access State must use the package
// mutex so concurrent callers (DHCP callbacks, gRPC handlers) don't race.
func UpdateIP(st *State, serviceKey string, ip net.IP) {
	mu.Lock()
	defer mu.Unlock()

	svc, ok := st.Services[serviceKey]
	if !ok {
		return
	}
	svc.SourceAddress = ip
	if ip != nil {
		svc.DHCPAcquired = true
	}
	st.UpdateCh <- LBNotification{
		Type:    UpdateEvent,
		Service: svc,
	}
}

func UpdateService(st *State, svc *LBService) {
	mu.Lock()
	defer mu.Unlock()

	eventType := UpdateEvent
	_, ok := st.Services[svc.ServiceName]
	if !ok {
		eventType = CreateEvent
	}

	st.Services[svc.ServiceName] = svc
	st.UpdateCh <- LBNotification{
		Type:    eventType,
		Service: svc,
	}
}

func DeleteService(st *State, serviceKey string) {
	mu.Lock()
	defer mu.Unlock()

	delete(st.Services, serviceKey)
	st.UpdateCh <- LBNotification{
		Type:    DeleteEvent,
		Service: &LBService{ServiceName: serviceKey},
	}
}

func HasService(st *State, serviceKey string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := st.Services[serviceKey]
	return ok
}

func GetService(st *State, serviceKey string) (*LBService, bool) {
	mu.RLock()
	defer mu.RUnlock()
	svc, ok := st.Services[serviceKey]
	return svc, ok
}
