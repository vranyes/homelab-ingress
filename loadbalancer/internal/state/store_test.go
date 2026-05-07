package state

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestStore_SaveAndLoad(t *testing.T) {
	t.Run("saves and loads state", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")

		st := NewState("wan.201")
		st.Services["default/myapp"] = &LBService{
			MACAddr:      "02:58:28:28:c3:f2",
			Interface:    "lb-default-myap",
			Ports:        []PortEntry{{Protocol: "tcp", ExternalPort: 443, PodPort: 443, PodIPs: []string{"10.42.0.10"}}},
			CurrentIP:    "81.23.45.67",
			DHCPAcquired: true,
		}

		store := NewStore(path)
		if err := store.Save(st); err != nil {
			t.Fatalf("unexpected error saving: %v", err)
		}

		loaded, err := store.Load()
		if err != nil {
			t.Fatalf("unexpected error loading: %v", err)
		}

		if loaded.BaseIface != "wan.201" {
			t.Errorf("expected base_iface wan.201, got %q", loaded.BaseIface)
		}
		if len(loaded.Services) != 1 {
			t.Fatalf("expected 1 service, got %d", len(loaded.Services))
		}
		svc := loaded.Services["default/myapp"]
		if svc.CurrentIP != "81.23.45.67" {
			t.Errorf("expected current_ip 81.23.45.67, got %q", svc.CurrentIP)
		}
		if !svc.DHCPAcquired {
			t.Error("expected dhcp_acquired to be true")
		}
		if len(svc.Ports) != 1 || svc.Ports[0].PodPort != 443 {
			t.Errorf("expected pod_port 443, got %+v", svc.Ports)
		}
	})

	t.Run("returns error when file does not exist", func(t *testing.T) {
		store := NewStore("/nonexistent/state.json")
		_, err := store.Load()
		if err == nil {
			t.Fatal("expected error for missing file, got nil")
		}
	})

	t.Run("returns error for invalid JSON", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		if err := os.WriteFile(path, []byte("not json"), 0644); err != nil {
			t.Fatal(err)
		}

		store := NewStore(path)
		_, err := store.Load()
		if err == nil {
			t.Fatal("expected error for invalid JSON, got nil")
		}
	})

	t.Run("creates parent directories on save", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "nested", "dir", "state.json")

		st := NewState("eth0")
		store := NewStore(path)
		if err := store.Save(st); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected state file to exist: %v", err)
		}
	})

	t.Run("overwrites existing file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")

		st := NewState("eth0")
		store := NewStore(path)
		if err := store.Save(st); err != nil {
			t.Fatal(err)
		}

		st.Services["test/svc"] = &LBService{
			MACAddr:   "02:00:00:00:00:01",
			Interface: "lb-test-svc",
			Ports:     []PortEntry{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.42.0.5"}}},
		}

		if err := store.Save(st); err != nil {
			t.Fatalf("unexpected error on second save: %v", err)
		}

		loaded, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := loaded.Services["test/svc"]; !ok {
			t.Fatal("expected test/svc service in loaded state")
		}
	})
}

func TestStore_UpdateIP(t *testing.T) {
	t.Run("updates IP for existing service", func(t *testing.T) {
		st := NewState("eth0")
		st.Services["default/myapp"] = &LBService{
			MACAddr:      "02:58:28:28:c3:f2",
			Interface:    "lb-default-myap",
			CurrentIP:    "81.23.45.67",
			DHCPAcquired: true,
		}

		UpdateIP(st, "default/myapp", "82.0.0.1")

		svc := st.Services["default/myapp"]
		if svc.CurrentIP != "82.0.0.1" {
			t.Errorf("expected IP 82.0.0.1, got %q", svc.CurrentIP)
		}
	})

	t.Run("does nothing for unknown service", func(t *testing.T) {
		st := NewState("eth0")
		UpdateIP(st, "unknown/svc", "1.2.3.4")
		if len(st.Services) != 0 {
			t.Errorf("expected no services, got %d", len(st.Services))
		}
	})
}

func TestStore_UpdateService(t *testing.T) {
	t.Run("creates new service entry", func(t *testing.T) {
		st := NewState("eth0")
		mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x28, 0xC3, 0xF2}
		ports := []PortEntry{{Protocol: "tcp", ExternalPort: 443, PodPort: 443, PodIPs: []string{"10.42.0.10"}}}

		UpdateService(st, "default/myapp", mac, "lb-default-myap", ports)

		if len(st.Services) != 1 {
			t.Fatalf("expected 1 service, got %d", len(st.Services))
		}
		svc := st.Services["default/myapp"]
		if svc.MACAddr != "02:58:28:28:c3:f2" {
			t.Errorf("expected MAC 02:58:28:28:c3:f2, got %q", svc.MACAddr)
		}
		if len(svc.Ports) != 1 {
			t.Errorf("expected 1 port, got %d", len(svc.Ports))
		}
	})

	t.Run("updates existing service", func(t *testing.T) {
		st := NewState("eth0")
		mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x28, 0xC3, 0xF2}
		ports := []PortEntry{{Protocol: "tcp", ExternalPort: 443, PodPort: 443, PodIPs: []string{"10.42.0.10"}}}
		UpdateService(st, "default/myapp", mac, "lb-default-myap", ports)

		newMac := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
		newPorts := []PortEntry{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.42.0.20", "10.42.0.21"}}}
		UpdateService(st, "default/myapp", newMac, "lb-default-myap", newPorts)

		svc := st.Services["default/myapp"]
		if len(svc.Ports) != 1 || svc.Ports[0].PodPort != 8080 {
			t.Errorf("expected updated ports, got %+v", svc.Ports)
		}
		if len(svc.Ports[0].PodIPs) != 2 {
			t.Errorf("expected 2 pod IPs, got %d", len(svc.Ports[0].PodIPs))
		}
	})
}

func TestStore_DeleteService(t *testing.T) {
	t.Run("removes service from state", func(t *testing.T) {
		st := NewState("eth0")
		st.Services["default/myapp"] = &LBService{
			MACAddr:   "02:58:28:28:c3:f2",
			Interface: "lb-default-myap",
			Ports:     []PortEntry{},
		}

		DeleteService(st, "default/myapp")

		if _, ok := st.Services["default/myapp"]; ok {
			t.Fatal("expected service to be deleted")
		}
	})

	t.Run("does nothing for unknown service", func(t *testing.T) {
		st := NewState("eth0")
		DeleteService(st, "unknown/svc")
	})
}

func TestStore_HasService(t *testing.T) {
	t.Run("returns true for existing service", func(t *testing.T) {
		st := NewState("eth0")
		st.Services["default/myapp"] = &LBService{}
		if !HasService(st, "default/myapp") {
			t.Fatal("expected HasService to return true")
		}
	})

	t.Run("returns false for missing service", func(t *testing.T) {
		st := NewState("eth0")
		if HasService(st, "default/myapp") {
			t.Fatal("expected HasService to return false")
		}
	})
}

func TestStore_GetService(t *testing.T) {
	t.Run("returns service when exists", func(t *testing.T) {
		st := NewState("eth0")
		svc := &LBService{}
		st.Services["default/myapp"] = svc

		got, ok := GetService(st, "default/myapp")
		if !ok {
			t.Fatal("expected GetService to return ok=true")
		}
		if got != svc {
			t.Error("expected same service pointer")
		}
	})

	t.Run("returns false when missing", func(t *testing.T) {
		st := NewState("eth0")
		_, ok := GetService(st, "default/myapp")
		if ok {
			t.Fatal("expected GetService to return ok=false")
		}
	})
}

func TestPortsEqual(t *testing.T) {
	tests := []struct {
		name string
		a    []PortEntry
		b    []PortEntry
		want bool
	}{
		{
			name: "equal ports",
			a:    []PortEntry{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}},
			b:    []PortEntry{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}},
			want: true,
		},
		{
			name: "different length",
			a:    []PortEntry{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}},
			b:    []PortEntry{},
			want: false,
		},
		{
			name: "different protocol",
			a:    []PortEntry{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}},
			b:    []PortEntry{{Protocol: "udp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}},
			want: false,
		},
		{
			name: "different pod IPs",
			a:    []PortEntry{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}},
			b:    []PortEntry{{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.2"}}},
			want: false,
		},
		{
			name: "both nil",
			a:    nil,
			b:    nil,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PortsEqual(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

func TestLBService_ParseMAC(t *testing.T) {
	t.Run("parses valid MAC", func(t *testing.T) {
		svc := &LBService{MACAddr: "02:58:28:28:c3:f2"}
		mac, err := svc.ParseMAC()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mac.String() != "02:58:28:28:c3:f2" {
			t.Errorf("expected 02:58:28:28:c3:f2, got %s", mac.String())
		}
	})

	t.Run("returns error for invalid MAC", func(t *testing.T) {
		svc := &LBService{MACAddr: "invalid"}
		_, err := svc.ParseMAC()
		if err == nil {
			t.Fatal("expected error for invalid MAC, got nil")
		}
	})
}

func TestLBService_SetMAC(t *testing.T) {
	svc := &LBService{}
	mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x28, 0xC3, 0xF2}
	svc.SetMAC(mac)
	if svc.MACAddr != "02:58:28:28:c3:f2" {
		t.Errorf("expected 02:58:28:28:c3:f2, got %q", svc.MACAddr)
	}
}

func TestNewState(t *testing.T) {
	st := NewState("eth0")
	if st.BaseIface != "eth0" {
		t.Errorf("expected base_iface eth0, got %q", st.BaseIface)
	}
	if st.Services == nil {
		t.Fatal("expected Services map to be initialized")
	}
	if st.Version != StateVersion {
		t.Errorf("expected version %d, got %d", StateVersion, st.Version)
	}
}

func TestStore_LoadNilServices(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	content := `{"version": 1, "base_interface": "eth0"}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	store := NewStore(path)
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded.Services == nil {
		t.Fatal("expected Services map to be initialized")
	}
}

func TestStore_SaveMkdirError(t *testing.T) {
	store := NewStore("/proc/impossible/path/state.json")
	err := store.Save(NewState("eth0"))
	if err == nil {
		t.Fatal("expected error for unwritable path")
	}
}

func TestPortEntryEqual(t *testing.T) {
	t.Run("equal entries", func(t *testing.T) {
		a := &PortEntry{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1", "10.0.0.2"}}
		b := &PortEntry{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1", "10.0.0.2"}}
		if !portEntryEqual(a, b) {
			t.Fatal("expected equal")
		}
	})

	t.Run("different pod port", func(t *testing.T) {
		a := &PortEntry{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}
		b := &PortEntry{Protocol: "tcp", ExternalPort: 80, PodPort: 9090, PodIPs: []string{"10.0.0.1"}}
		if portEntryEqual(a, b) {
			t.Fatal("expected not equal")
		}
	})

	t.Run("different pod IP count", func(t *testing.T) {
		a := &PortEntry{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1"}}
		b := &PortEntry{Protocol: "tcp", ExternalPort: 80, PodPort: 8080, PodIPs: []string{"10.0.0.1", "10.0.0.2"}}
		if portEntryEqual(a, b) {
			t.Fatal("expected not equal")
		}
	})
}
