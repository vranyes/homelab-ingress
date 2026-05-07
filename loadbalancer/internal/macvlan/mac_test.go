package macvlan

import (
	"net"
	"strings"
	"testing"
)

func TestGenerateMAC(t *testing.T) {
	t.Run("generates MAC with local bit set", func(t *testing.T) {
		mac := GenerateMAC()
		if mac[0]&0x02 == 0 {
			t.Errorf("expected local bit set, got MAC %v", mac)
		}
	})

	t.Run("generates unique MACs", func(t *testing.T) {
		seen := make(map[string]bool)
		for i := 0; i < 1000; i++ {
			mac := GenerateMAC()
			s := mac.String()
			if seen[s] {
				t.Fatalf("duplicate MAC generated: %s", s)
			}
			seen[s] = true
		}
	})

	t.Run("generates valid 6-byte MAC", func(t *testing.T) {
		mac := GenerateMAC()
		if len(mac) != 6 {
			t.Errorf("expected 6-byte MAC, got %d bytes", len(mac))
		}
	})
}

func TestParseMAC(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:    "valid colon-separated MAC",
			input:   "00:58:28:28:C3:F2",
			wantErr: false,
		},
		{
			name:    "valid lowercase MAC",
			input:   "00:58:28:28:c3:f2",
			wantErr: false,
		},
		{
			name:    "invalid format",
			input:   "invalid",
			wantErr: true,
		},
		{
			name:    "too short",
			input:   "00:58:28",
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mac, err := ParseMAC(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mac.String() != strings.ToLower(tt.input) {
				t.Errorf("expected %q, got %q", strings.ToLower(tt.input), mac.String())
			}
		})
	}
}

func TestInterfaceName(t *testing.T) {
	tests := []struct {
		name       string
		serviceKey string
		want       string
	}{
		{
			name:       "simple service key",
			serviceKey: "default/myapp",
			want:       "lb-defau-fcb209",
		},
		{
			name:       "long service key gets truncated with hash",
			serviceKey: "very-long-namespace/very-long-service-name-that-exceeds-limit",
			want:       "lb-very-88f5a8",
		},
		{
			name:       "single segment key",
			serviceKey: "myservice",
			want:       "lb-myser-46153b",
		},
		{
			// Keys that share the same 5-char truncated prefix must still differ via hash.
			name:       "collision-prone key 1",
			serviceKey: "ns/svc-abc",
			want:       "lb-ns-sv-d8f182",
		},
		{
			name:       "collision-prone key 2",
			serviceKey: "ns/svc-abcd",
			want:       "lb-ns-sv-006bd3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InterfaceName(tt.serviceKey)
			if got != tt.want {
				t.Errorf("expected %q, got %q", tt.want, got)
			}
			if len(got) > maxInterfaceNameLen {
				t.Errorf("interface name too long: %q (%d chars)", got, len(got))
			}
		})
	}

	t.Run("deterministic", func(t *testing.T) {
		key := "default/myapp"
		if InterfaceName(key) != InterfaceName(key) {
			t.Error("InterfaceName is not deterministic")
		}
	})

	t.Run("different keys produce different names", func(t *testing.T) {
		seen := make(map[string]string)
		keys := []string{
			"default/myapp",
			"default/myapp2",
			"ns/svc-abc",
			"ns/svc-abcd",
			"very-long-namespace/very-long-service-name-that-exceeds-limit",
			"very-long-namespace/another-very-long-service-name-that-exceeds-limit",
		}
		for _, k := range keys {
			name := InterfaceName(k)
			if prev, ok := seen[name]; ok {
				t.Errorf("collision: %q and %q both produce %q", prev, k, name)
			}
			seen[name] = k
		}
	})
}

func TestMACString(t *testing.T) {
	mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x28, 0xC3, 0xF2}
	s := MACString(mac)
	if s != "02:58:28:28:c3:f2" {
		t.Errorf("expected 02:58:28:28:c3:f2, got %q", s)
	}
}

func TestValidateMAC(t *testing.T) {
	t.Run("valid MAC with local bit set", func(t *testing.T) {
		mac := net.HardwareAddr{0x02, 0x58, 0x28, 0x28, 0xC3, 0xF2}
		if err := ValidateMAC(mac); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("nil MAC is invalid", func(t *testing.T) {
		if err := ValidateMAC(nil); err == nil {
			t.Fatal("expected error for nil MAC, got nil")
		}
	})

	t.Run("MAC with wrong length is invalid", func(t *testing.T) {
		mac := net.HardwareAddr{0x02, 0x58}
		if err := ValidateMAC(mac); err == nil {
			t.Fatal("expected error for short MAC, got nil")
		}
	})
}
