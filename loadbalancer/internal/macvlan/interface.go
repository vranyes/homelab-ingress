package macvlan

import (
	"errors"
	"net"
)

var (
	ErrInterfaceExists   = errors.New("interface already exists")
	ErrInterfaceNotFound = errors.New("interface not found")
)

type LinkManager interface {
	Create(iface, parent string, mac net.HardwareAddr) error
	Delete(iface string) error
	Exists(iface string) bool
	GetMAC(iface string) (net.HardwareAddr, error)
	SetUp(iface string) error
}
