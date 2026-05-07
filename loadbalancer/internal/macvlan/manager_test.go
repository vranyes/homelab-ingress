package macvlan

import (
	"net"
	"testing"
)

type mockLinkManager struct {
	links map[string]*mockLink
}

type mockLink struct {
	name string
	mac  net.HardwareAddr
	up   bool
}

func newMockManager() *mockLinkManager {
	return &mockLinkManager{
		links: make(map[string]*mockLink),
	}
}

func (m *mockLinkManager) Create(iface, parent string, mac net.HardwareAddr) error {
	if _, exists := m.links[iface]; exists {
		return ErrInterfaceExists
	}
	m.links[iface] = &mockLink{
		name: iface,
		mac:  mac,
	}
	return nil
}

func (m *mockLinkManager) Delete(iface string) error {
	delete(m.links, iface)
	return nil
}

func (m *mockLinkManager) Exists(iface string) bool {
	_, ok := m.links[iface]
	return ok
}

func (m *mockLinkManager) GetMAC(iface string) (net.HardwareAddr, error) {
	link, ok := m.links[iface]
	if !ok {
		return nil, ErrInterfaceNotFound
	}
	return link.mac, nil
}

func (m *mockLinkManager) SetUp(iface string) error {
	link, ok := m.links[iface]
	if !ok {
		return ErrInterfaceNotFound
	}
	link.up = true
	return nil
}

func TestManager_Create(t *testing.T) {
	mgr := newMockManager()
	mac := GenerateMAC()

	t.Run("creates new interface", func(t *testing.T) {
		err := mgr.Create("lb-test", "wan.201", mac)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mgr.Exists("lb-test") {
			t.Fatal("expected interface to exist after create")
		}
	})

	t.Run("returns error for duplicate interface", func(t *testing.T) {
		err := mgr.Create("lb-test", "wan.201", mac)
		if err != ErrInterfaceExists {
			t.Fatalf("expected ErrInterfaceExists, got %v", err)
		}
	})
}

func TestManager_Delete(t *testing.T) {
	mgr := newMockManager()
	mac := GenerateMAC()

	t.Run("deletes existing interface", func(t *testing.T) {
		_ = mgr.Create("lb-todelete", "wan.201", mac)
		if err := mgr.Delete("lb-todelete"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mgr.Exists("lb-todelete") {
			t.Fatal("expected interface to be deleted")
		}
	})

	t.Run("deleting non-existent interface is a no-op", func(t *testing.T) {
		err := mgr.Delete("lb-nonexistent")
		if err != nil {
			t.Fatalf("expected no error for deleting non-existent interface, got %v", err)
		}
	})
}

func TestManager_GetMAC(t *testing.T) {
	mgr := newMockManager()
	mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x28, 0xC3, 0xF2}
	_ = mgr.Create("lb-getmac", "wan.201", mac)

	t.Run("returns MAC for existing interface", func(t *testing.T) {
		got, err := mgr.GetMAC("lb-getmac")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.String() != mac.String() {
			t.Errorf("expected MAC %s, got %s", mac.String(), got.String())
		}
	})

	t.Run("returns error for non-existent interface", func(t *testing.T) {
		_, err := mgr.GetMAC("lb-nonexistent")
		if err != ErrInterfaceNotFound {
			t.Fatalf("expected ErrInterfaceNotFound, got %v", err)
		}
	})
}

func TestManager_SetUp(t *testing.T) {
	mgr := newMockManager()
	mac := GenerateMAC()

	t.Run("sets up existing interface", func(t *testing.T) {
		_ = mgr.Create("lb-setup", "wan.201", mac)
		if err := mgr.SetUp("lb-setup"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !mgr.links["lb-setup"].up {
			t.Fatal("expected interface to be up")
		}
	})

	t.Run("returns error for non-existent interface", func(t *testing.T) {
		err := mgr.SetUp("lb-nonexistent")
		if err != ErrInterfaceNotFound {
			t.Fatalf("expected ErrInterfaceNotFound, got %v", err)
		}
	})
}

func TestManager_Exists(t *testing.T) {
	mgr := newMockManager()

	if mgr.Exists("lb-nonexistent") {
		t.Fatal("expected non-existent interface to not exist")
	}

	mac := GenerateMAC()
	_ = mgr.Create("lb-exists", "wan.201", mac)

	if !mgr.Exists("lb-exists") {
		t.Fatal("expected existing interface to exist")
	}
}
