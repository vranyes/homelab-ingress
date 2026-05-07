package macvlan

import (
	"net"
	"strings"
	"testing"
)

func TestGenerateMAC(t *testing.T) {
	mac := GenerateMAC()
	if len(mac) != 6 {
		t.Fatalf("expected 6 bytes, got %d", len(mac))
	}
	if mac[0]&0x02 == 0 {
		t.Error("expected locally administered bit to be set")
	}
	if mac[0]&0x01 != 0 {
		t.Error("expected unicast bit to be set (not multicast)")
	}
}

func TestGenerateMACUniqueness(t *testing.T) {
	const n = 100
	seen := make(map[string]bool)
	for i := 0; i < n; i++ {
		mac := GenerateMAC()
		s := mac.String()
		if seen[s] {
			t.Fatalf("duplicate MAC generated: %s", s)
		}
		seen[s] = true
	}
}

func TestInterfaceNameFormat(t *testing.T) {
	name := InterfaceName("default/my-service")
	if !strings.HasPrefix(name, "lb-") {
		t.Errorf("expected 'lb-' prefix, got %q", name)
	}
	if len(name) > 15 {
		t.Errorf("interface name exceeds 15 characters: %q (%d)", name, len(name))
	}
	if !strings.Contains(name, "-") {
		t.Error("expected separator in interface name")
	}
}

func TestInterfaceNameSlashNormalization(t *testing.T) {
	name := InterfaceName("namespace/service")
	if strings.Contains(name, "/") {
		t.Errorf("interface name should not contain '/': %q", name)
	}
}

func TestInterfaceNameMaxLength(t *testing.T) {
	longKey := strings.Repeat("a", 100)
	name := InterfaceName(longKey)
	if len(name) > 15 {
		t.Errorf("interface name exceeds 15 characters: %q (%d)", name, len(name))
	}
	if !strings.HasPrefix(name, "lb-") {
		t.Errorf("expected 'lb-' prefix, got %q", name)
	}
}

func TestInterfaceNameDeterministic(t *testing.T) {
	k1 := InterfaceName("default/my-app")
	k2 := InterfaceName("default/my-app")
	if k1 != k2 {
		t.Errorf("expected deterministic names, got %q vs %q", k1, k2)
	}
}

func TestInterfaceNameDifferentKeys(t *testing.T) {
	a := InterfaceName("default/app-one")
	b := InterfaceName("default/app-two")
	if a == b {
		t.Error("expected different names for different keys")
	}
}

func TestInterfaceNameNoTrailingDash(t *testing.T) {
	longKey := strings.Repeat("x", 50)
	name := InterfaceName(longKey)
	if strings.HasSuffix(strings.TrimPrefix(name, "lb-"), "-") {
		t.Errorf("interface name should not end with '-': %q", name)
	}
}

func TestGenerateMACValidHardwareAddr(t *testing.T) {
	mac := GenerateMAC()
	var zeroMAC net.HardwareAddr = make(net.HardwareAddr, 6)
	if mac.String() == zeroMAC.String() {
		t.Error("MAC address should not be all zeros")
	}
}
