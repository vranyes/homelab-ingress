package firewall

import (
	"strings"
	"testing"
)

func TestChainName(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"default/myapp", "ingress_default_myapp"},
		{"test/svc-1", "ingress_test_svc_1"},
		{"very/long-service-name-that-should-be-truncated", "ingress_very_long_service_name_that_sh"},
		{"a/b", "ingress_a_b"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := ingressChainName(tt.key); got != tt.want {
				t.Errorf("chainName(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestNewNoOpManager(t *testing.T) {
	mgr := NewNoOpManager()
	if mgr == nil {
		t.Fatal("expected non-nil manager")
	}
	if err := mgr.AddServiceRules("eth0", "1.2.3.4", 80, "tcp", []string{"10.0.0.1"}, 8080, "default/app"); err != nil {
		t.Fatalf("AddServiceRules on no-op manager: %v", err)
	}
	if err := mgr.RemoveRules("default/app"); err != nil {
		t.Fatalf("RemoveRules on no-op manager: %v", err)
	}
	if err := mgr.RemoveAllRules(); err != nil {
		t.Fatalf("RemoveAllRules on no-op manager: %v", err)
	}
}

func TestAddRules_NoPods(t *testing.T) {
	mgr := NewNoOpManager()
	err := mgr.AddServiceRules("eth0", "1.2.3.4", 80, "tcp", nil, 8080, "default/app")
	if err == nil {
		t.Fatal("expected error for empty pod IPs")
	}
	if !strings.Contains(err.Error(), "no pod IPs") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestAddServiceRules_NoPorts(t *testing.T) {
	mgr := NewNoOpManager()
	if err := mgr.AddServiceRules("eth0", "", nil, "default/app"); err == nil {
		t.Fatal("expected error for nil ports")
	}
}

func TestAddServiceRules_NoPodIPs(t *testing.T) {
	mgr := NewNoOpManager()
	err := mgr.AddServiceRules("eth0", "", []PortRule{{ExtPort: 80, Protocol: "tcp"}}, "default/app")
	if err == nil {
		t.Fatal("expected error for port with no pod IPs")
	}
}

func TestAddServiceRules_MultiPort_NoOp(t *testing.T) {
	mgr := NewNoOpManager()
	err := mgr.AddServiceRules("eth0", "1.2.3.4", []PortRule{
		{ExtPort: 80, Protocol: "tcp", PodIPs: []string{"10.0.0.1"}, PodPort: 8080},
		{ExtPort: 443, Protocol: "tcp", PodIPs: []string{"10.0.0.1", "10.0.0.2"}, PodPort: 8443},
		{ExtPort: 53, Protocol: "udp", PodIPs: []string{"10.0.0.1"}, PodPort: 5353},
	}, "default/app")
	if err != nil {
		t.Fatalf("AddServiceRules on no-op manager: %v", err)
	}
}
