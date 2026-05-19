//go:build linux

package dhcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
)

type RealClient struct {
	mu       sync.RWMutex
	iface    string
	mac      net.HardwareAddr
	ip       net.IP
	lease    *nclient4.Lease
	acquired bool
	cancel   context.CancelFunc
	done     chan struct{}
	onChange IPChangeFunc
	logger   *slog.Logger
}

func NewRealClient(iface string, mac net.HardwareAddr, onChange IPChangeFunc) *RealClient {
	return &RealClient{
		iface:    iface,
		mac:      mac,
		onChange: onChange,
		done:     make(chan struct{}),
		logger:   slog.Default(),
	}
}

func NewRealClientWithLogger(iface string, mac net.HardwareAddr, onChange IPChangeFunc, logger *slog.Logger) *RealClient {
	return &RealClient{
		iface:    iface,
		mac:      mac,
		onChange: onChange,
		done:     make(chan struct{}),
		logger:   logger,
	}
}

func (c *RealClient) Start() error {
	c.logger.Info("starting DHCP client", "interface", c.iface, "mac", c.mac)

	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()

	go c.run(ctx)
	return nil
}

func (c *RealClient) Stop() {
	c.logger.Info("stopping DHCP client", "interface", c.iface)

	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()
	<-c.done

	if err := c.release(); err != nil {
		c.logger.Error("failed to release DHCP lease", "interface", c.iface, "error", err)
	}

	c.logger.Info("DHCP client stopped", "interface", c.iface)
}

func (c *RealClient) CurrentIP() net.IP {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ip
}

func (c *RealClient) IsAcquired() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.acquired
}

func (c *RealClient) run(ctx context.Context) {
	defer close(c.done)

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("DHCP client context cancelled", "interface", c.iface)
			return
		default:
		}

		ip, err := c.dhcpRequest(ctx)
		if err != nil {
			c.logger.Error("DHCP request failed", "interface", c.iface, "error", err)
			select {
			case <-ctx.Done():
				c.logger.Info("DHCP client context cancelled", "interface", c.iface)
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		c.mu.Lock()
		changed := c.ip == nil || !c.ip.Equal(ip)
		firstAcquisition := !c.acquired
		c.ip = ip
		c.acquired = true
		c.mu.Unlock()

		if changed && c.onChange != nil {
			c.logger.Info("IP changed, calling onChange callback", "interface", c.iface, "new_ip", ip, "first_acquisition", firstAcquisition)
			c.onChange(c.iface, ip)
		}

		c.logger.Info("DHCP lease acquired", "interface", c.iface, "ip", ip)

		// Renewal loop: renew at T1 (half the lease time) using a unicast
		// REQUEST to the server rather than re-discovering, which would cause
		// the server to assign a new IP.
		for {
			renewalInterval := c.renewalInterval()
			c.logger.Debug("scheduling DHCP renewal", "interface", c.iface, "in", renewalInterval)
			select {
			case <-ctx.Done():
				c.logger.Info("DHCP client context cancelled", "interface", c.iface)
				return
			case <-time.After(renewalInterval):
			}

			newIP, err := c.dhcpRenew(ctx)
			if err != nil {
				c.logger.Warn("DHCP renewal failed, will re-discover", "interface", c.iface, "error", err)
				break // break inner loop; outer loop will do a full re-discover
			}

			c.mu.Lock()
			renewalChanged := c.ip == nil || !c.ip.Equal(newIP)
			c.ip = newIP
			c.mu.Unlock()

			if renewalChanged && c.onChange != nil {
				c.logger.Info("IP changed after renewal", "interface", c.iface, "new_ip", newIP)
				c.onChange(c.iface, newIP)
			}
			c.logger.Info("DHCP lease renewed", "interface", c.iface, "ip", newIP)
		}
	}
}

func (c *RealClient) dhcpRequest(ctx context.Context) (net.IP, error) {
	c.logger.Info("sending DHCP discover/request", "interface", c.iface, "mac", c.mac)

	client, err := nclient4.New(c.iface,
		nclient4.WithTimeout(30*time.Second),
		nclient4.WithRetry(3),
	)
	if err != nil {
		c.logger.Error("failed to create DHCP client", "interface", c.iface, "error", err)
		return nil, err
	}
	defer client.Close()

	resp, err := client.Request(ctx)
	if err != nil {
		c.logger.Error("DHCP request failed", "interface", c.iface, "error", err)
		return nil, err
	}

	ip := resp.ACK.YourIPAddr
	c.logger.Info("DHCP ACK received", "interface", c.iface, "ip", ip)

	c.mu.Lock()
	c.lease = resp
	c.mu.Unlock()

	return ip, nil
}

// dhcpRenew sends a unicast DHCPREQUEST to renew the current lease without
// triggering a new DISCOVER, which would cause the server to hand out a
// different IP address.
func (c *RealClient) dhcpRenew(ctx context.Context) (net.IP, error) {
	c.mu.RLock()
	lease := c.lease
	c.mu.RUnlock()

	if lease == nil {
		return nil, fmt.Errorf("no active lease to renew")
	}

	c.logger.Info("sending DHCP renewal", "interface", c.iface)

	client, err := nclient4.New(c.iface,
		nclient4.WithTimeout(30*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create DHCP client for renewal: %w", err)
	}
	defer client.Close()

	resp, err := client.Renew(ctx, lease)
	if err != nil {
		return nil, fmt.Errorf("DHCP renewal failed: %w", err)
	}

	ip := resp.ACK.YourIPAddr
	c.logger.Info("DHCP renewal ACK received", "interface", c.iface, "ip", ip)

	c.mu.Lock()
	c.lease = resp
	c.mu.Unlock()

	return ip, nil
}

// renewalInterval returns T1: half the lease time from the current lease,
// clamped to a minimum of 30 seconds. Falls back to 30 minutes if no lease
// time option is present in the ACK.
func (c *RealClient) renewalInterval() time.Duration {
	const minInterval = 30 * time.Second
	const defaultLeaseTime = 60 * time.Minute

	c.mu.RLock()
	lease := c.lease
	c.mu.RUnlock()

	if lease == nil || lease.ACK == nil {
		return defaultLeaseTime / 2
	}

	leaseTime := lease.ACK.IPAddressLeaseTime(0)
	if leaseTime <= 0 {
		leaseTime = defaultLeaseTime
	}

	t1 := leaseTime / 2
	if t1 < minInterval {
		return minInterval
	}
	return t1
}

func (c *RealClient) release() error {
	c.mu.RLock()
	ip := c.ip
	lease := c.lease
	c.mu.RUnlock()

	if ip == nil || lease == nil {
		c.logger.Debug("no active lease to release", "interface", c.iface)
		return nil
	}

	c.logger.Info("releasing DHCP lease", "interface", c.iface, "ip", ip)

	client, err := nclient4.New(c.iface,
		nclient4.WithTimeout(10*time.Second),
	)
	if err != nil {
		return fmt.Errorf("failed to create DHCP client for release: %w", err)
	}
	defer client.Close()

	if err := client.Release(lease); err != nil {
		return fmt.Errorf("failed to send DHCP release: %w", err)
	}
	return nil
}

// subnetMaskFromACK returns the subnet mask from a DHCP ACK option, or falls
// back to /24 if the option is absent.
func subnetMaskFromACK(ack *dhcpv4.DHCPv4) net.IPMask {
	if mask := ack.SubnetMask(); mask != nil {
		return mask
	}
	return net.CIDRMask(24, 32)
}
