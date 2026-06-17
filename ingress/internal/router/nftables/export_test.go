//go:build linux

package nftables

import (
	"log/slog"

	"github.com/google/nftables"
)

// NewManagerWithConn creates a Manager with a given nftables connection,
// for use in tests that mock netlink via nftables.WithTestDial.
func NewManagerWithConn(conn *nftables.Conn, logger *slog.Logger) *Manager {
	return &Manager{conn: conn, logger: logger}
}
