package macvlan

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"net"
	"strings"
)

const (
	maxInterfaceNameLen = 15
	hashSuffixLen       = 8
)

func GenerateMAC() net.HardwareAddr {
	mac := make(net.HardwareAddr, 6)
	_, _ = rand.Read(mac)
	mac[0] = (mac[0] & 0xFE) | 0x02
	return mac
}

func InterfaceName(serviceKey string) string {
	normalized := strings.ReplaceAll(serviceKey, "/", "-")
	h := sha1.Sum([]byte(serviceKey))
	hash := hex.EncodeToString(h[:])[:hashSuffixLen]
	maxShortLen := maxInterfaceNameLen - 3 - 1 - hashSuffixLen
	short := normalized
	if len(short) > maxShortLen {
		short = strings.TrimRight(short[:maxShortLen], "-")
	}
	return "lb-" + short + "-" + hash
}
