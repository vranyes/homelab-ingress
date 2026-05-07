package dhcp

import (
	"net"
	"sync"
	"testing"
)

type mockClient struct {
	mu       sync.Mutex
	ip       net.IP
	acquired bool
	started  bool
	stopped  bool
	startErr error
}

func (m *mockClient) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = true
	return m.startErr
}

func (m *mockClient) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = true
}

func (m *mockClient) CurrentIP() net.IP {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ip
}

func (m *mockClient) IsAcquired() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acquired
}

func (m *mockClient) setIP(ip net.IP) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ip = ip
	m.acquired = ip != nil
}

func TestManager_AddAndGet(t *testing.T) {
	mgr := NewManager(nil)
	client := &mockClient{}

	mgr.Add("lb-test", client)

	got, ok := mgr.Get("lb-test")
	if !ok {
		t.Fatal("expected to get client")
	}
	if got != client {
		t.Fatal("expected same client pointer")
	}
}

func TestManager_Remove(t *testing.T) {
	mgr := NewManager(nil)
	client := &mockClient{}
	mgr.Add("lb-test", client)

	mgr.Remove("lb-test")

	_, ok := mgr.Get("lb-test")
	if ok {
		t.Fatal("expected client to be removed")
	}
	if !client.stopped {
		t.Fatal("expected client to be stopped on remove")
	}
}

func TestManager_RemoveNonExistent(t *testing.T) {
	mgr := NewManager(nil)
	mgr.Remove("lb-nonexistent")
}

func TestManager_CurrentIP(t *testing.T) {
	mgr := NewManager(nil)
	client := &mockClient{}
	client.setIP(net.ParseIP("81.23.45.67"))
	mgr.Add("lb-test", client)

	t.Run("returns IP when acquired", func(t *testing.T) {
		ip, ok := mgr.CurrentIP("lb-test")
		if !ok {
			t.Fatal("expected ok=true")
		}
		if ip.String() != "81.23.45.67" {
			t.Errorf("expected 81.23.45.67, got %s", ip.String())
		}
	})

	t.Run("returns false for unknown interface", func(t *testing.T) {
		_, ok := mgr.CurrentIP("lb-unknown")
		if ok {
			t.Fatal("expected ok=false for unknown interface")
		}
	})
}

func TestManager_IsAcquired(t *testing.T) {
	mgr := NewManager(nil)
	client := &mockClient{}
	mgr.Add("lb-test", client)

	t.Run("returns false when not acquired", func(t *testing.T) {
		if mgr.IsAcquired("lb-test") {
			t.Fatal("expected not acquired")
		}
	})

	t.Run("returns true when acquired", func(t *testing.T) {
		client.setIP(net.ParseIP("81.23.45.67"))
		if !mgr.IsAcquired("lb-test") {
			t.Fatal("expected acquired")
		}
	})

	t.Run("returns false for unknown interface", func(t *testing.T) {
		if mgr.IsAcquired("lb-unknown") {
			t.Fatal("expected false for unknown interface")
		}
	})
}

func TestManager_StopAll(t *testing.T) {
	mgr := NewManager(nil)
	c1 := &mockClient{}
	c2 := &mockClient{}
	mgr.Add("lb-one", c1)
	mgr.Add("lb-two", c2)

	mgr.StopAll()

	if !c1.stopped || !c2.stopped {
		t.Fatal("expected all clients to be stopped")
	}
	if len(mgr.clients) != 0 {
		t.Fatalf("expected clients map to be empty, got %d", len(mgr.clients))
	}
}

func TestManager_ListInterfaces(t *testing.T) {
	mgr := NewManager(nil)

	t.Run("returns empty list when no clients", func(t *testing.T) {
		list := mgr.ListInterfaces()
		if len(list) != 0 {
			t.Errorf("expected empty list, got %v", list)
		}
	})

	t.Run("returns interface names", func(t *testing.T) {
		mgr.Add("lb-one", &mockClient{})
		mgr.Add("lb-two", &mockClient{})

		list := mgr.ListInterfaces()
		if len(list) != 2 {
			t.Fatalf("expected 2 interfaces, got %d", len(list))
		}
	})
}

func TestManager_OnChangeCallback(t *testing.T) {
	var called []struct {
		iface string
		ip    net.IP
	}

	mgr := NewManager(func(iface string, ip net.IP) {
		called = append(called, struct {
			iface string
			ip    net.IP
		}{iface, ip})
	})

	client := &mockClient{}
	mgr.Add("lb-test", client)
	client.setIP(net.ParseIP("81.23.45.67"))

	ip, _ := mgr.CurrentIP("lb-test")
	if ip != nil {
		mgr.onChange("lb-test", ip)
	}

	if len(called) != 1 {
		t.Fatalf("expected 1 callback call, got %d", len(called))
	}
	if called[0].iface != "lb-test" {
		t.Errorf("expected iface lb-test, got %q", called[0].iface)
	}
	if called[0].ip.String() != "81.23.45.67" {
		t.Errorf("expected IP 81.23.45.67, got %s", called[0].ip.String())
	}
}

func TestNewManager(t *testing.T) {
	mgr := NewManager(nil)
	if mgr.clients == nil {
		t.Fatal("expected clients map to be initialized")
	}
}

func TestNewManagerWithFactory(t *testing.T) {
	factory := func(iface string, mac net.HardwareAddr, onChange IPChangeFunc) Client {
		return &mockClient{}
	}

	mgr := NewManagerWithFactory(func(iface string, ip net.IP) {}, factory)
	if mgr == nil {
		t.Fatal("expected manager")
	}
	if mgr.newClient == nil {
		t.Fatal("expected factory to be set")
	}
}

func TestManager_NewAndAdd(t *testing.T) {
	t.Run("creates and adds client via factory", func(t *testing.T) {
		factory := func(iface string, mac net.HardwareAddr, onChange IPChangeFunc) Client {
			return &mockClient{}
		}

		mgr := NewManagerWithFactory(func(iface string, ip net.IP) {}, factory)
		mac := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
		mgr.NewAndAdd("lb-test", mac)

		client, ok := mgr.Get("lb-test")
		if !ok {
			t.Fatal("expected client to be added")
		}
		if client == nil {
			t.Fatal("expected non-nil client")
		}
	})

	t.Run("does nothing when no factory set", func(t *testing.T) {
		mgr := NewManager(nil)
		mac := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
		mgr.NewAndAdd("lb-test", mac)

		_, ok := mgr.Get("lb-test")
		if ok {
			t.Fatal("expected no client when factory is nil")
		}
	})
}

func TestManager_CurrentIP_NilIP(t *testing.T) {
	mgr := NewManager(nil)
	client := &mockClient{}
	mgr.Add("lb-test", client)

	_, ok := mgr.CurrentIP("lb-test")
	if ok {
		t.Fatal("expected ok=false when IP is nil")
	}
}

func TestManager_Start(t *testing.T) {
	t.Run("starts client for interface", func(t *testing.T) {
		mgr := NewManager(nil)
		client := &mockClient{}
		mgr.Add("lb-test", client)

		if err := mgr.Start("lb-test"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !client.started {
			t.Fatal("expected client to be started")
		}
	})

	t.Run("returns error for unknown interface", func(t *testing.T) {
		mgr := NewManager(nil)
		err := mgr.Start("lb-unknown")
		if err == nil {
			t.Fatal("expected error for unknown interface")
		}
	})

	t.Run("returns start error from client", func(t *testing.T) {
		mgr := NewManager(nil)
		client := &mockClient{startErr: net.ErrClosed}
		mgr.Add("lb-test", client)

		err := mgr.Start("lb-test")
		if err == nil {
			t.Fatal("expected error from client Start")
		}
	})
}

func TestManager_StartAll(t *testing.T) {
	t.Run("starts all clients", func(t *testing.T) {
		mgr := NewManager(nil)
		c1 := &mockClient{}
		c2 := &mockClient{}
		mgr.Add("lb-one", c1)
		mgr.Add("lb-two", c2)

		if err := mgr.StartAll(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !c1.started || !c2.started {
			t.Fatal("expected all clients to be started")
		}
	})

	t.Run("returns error when one client fails", func(t *testing.T) {
		mgr := NewManager(nil)
		c1 := &mockClient{}
		c2 := &mockClient{startErr: net.ErrClosed}
		mgr.Add("lb-one", c1)
		mgr.Add("lb-two", c2)

		err := mgr.StartAll()
		if err == nil {
			t.Fatal("expected error when one client fails")
		}
		if !c1.started {
			t.Fatal("expected successful client to still be started")
		}
	})

	t.Run("returns nil when no clients", func(t *testing.T) {
		mgr := NewManager(nil)
		if err := mgr.StartAll(); err != nil {
			t.Fatalf("expected nil error, got: %v", err)
		}
	})
}
