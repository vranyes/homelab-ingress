package macvlan

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
)

const (
	maxInterfaceNameLen = 15
	hashSuffixLen       = 6
)

func GenerateMAC() net.HardwareAddr {
	mac := make(net.HardwareAddr, 6)
	_, _ = rand.Read(mac)
	mac[0] = (mac[0] & 0xFE) | 0x02
	return mac
}

func ParseMAC(s string) (net.HardwareAddr, error) {
	mac, err := net.ParseMAC(s)
	if err != nil {
		return nil, fmt.Errorf("invalid MAC address %q: %w", s, err)
	}
	if len(mac) != 6 {
		return nil, fmt.Errorf("MAC address must be 6 bytes, got %d", len(mac))
	}
	return mac, nil
}

func ValidateMAC(mac net.HardwareAddr) error {
	if mac == nil {
		return errors.New("MAC address is nil")
	}
	if len(mac) != 6 {
		return fmt.Errorf("MAC address must be 6 bytes, got %d", len(mac))
	}
	return nil
}

func InterfaceName(serviceKey string) string {
	normalized := strings.ReplaceAll(serviceKey, "/", "-")
	h := sha1.Sum([]byte(serviceKey))
	hash := hex.EncodeToString(h[:])[:hashSuffixLen]
	// Format: "lb-" + shortName + "-" + hash6, total <= 15.
	// Available for shortName: 15 - len("lb-") - len("-") - hashSuffixLen = 5.
	maxShortLen := maxInterfaceNameLen - 3 - 1 - hashSuffixLen
	short := normalized
	if len(short) > maxShortLen {
		short = strings.TrimRight(short[:maxShortLen], "-")
	}
	return "lb-" + short + "-" + hash
}

func MACString(mac net.HardwareAddr) string {
	return strings.ToLower(mac.String())
}
