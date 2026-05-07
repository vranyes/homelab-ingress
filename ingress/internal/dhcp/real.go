//go:build linux

package dhcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
)

const initialRetryInterval = 5 * time.Second

type realClient struct {
	mu       sync.RWMutex
	iface    string
	mac      net.HardwareAddr
	ip       net.IP
	mask     net.IPMask
	lease    *nclient4.Lease
	acquired bool
	cancel   context.CancelFunc
	done     chan struct{}
	started  bool
	onChange IPChangeFunc
	logger   *slog.Logger
}

func newRealClient(iface string, mac net.HardwareAddr, onChange IPChangeFunc, logger *slog.Logger) *realClient {
	return &realClient{
		iface:    iface,
		mac:      mac,
		onChange: onChange,
		done:     make(chan struct{}),
		logger:   logger,
	}
}

func (c *realClient) Start() error {
	c.logger.Info("starting DHCP client", "interface", c.iface, "mac", c.mac)

	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		c.logger.Debug("DHCP client already started", "interface", c.iface)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.started = true
	c.mu.Unlock()

	go c.run(ctx)
	return nil
}

func (c *realClient) Stop() {
	c.logger.Info("stopping DHCP client", "interface", c.iface)

	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		c.logger.Debug("DHCP client was never started, nothing to stop", "interface", c.iface)
		return
	}
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

func (c *realClient) CurrentIP() net.IP {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ip
}

func (c *realClient) IsAcquired() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.acquired
}

func (c *realClient) run(ctx context.Context) {
	defer close(c.done)

	for {
		ip, mask, err := c.dhcpRequest(ctx)
		if err != nil {
			c.logger.Error("DHCP request failed", "interface", c.iface, "error", err)
			select {
			case <-ctx.Done():
				c.logger.Info("DHCP client context cancelled", "interface", c.iface)
				return
			case <-time.After(initialRetryInterval):
				continue
			}
		}

		c.mu.Lock()
		changed := c.ip == nil || !c.ip.Equal(ip)
		c.ip = ip
		c.mask = mask
		c.acquired = true
		c.mu.Unlock()

		if changed && c.onChange != nil {
			c.logger.Info("IP changed, calling onChange callback", "interface", c.iface, "new_ip", ip)
			c.onChange(c.iface, ip, mask)
		}

		c.logger.Info("DHCP lease acquired", "interface", c.iface, "ip", ip)

		for {
			renewalInterval := c.renewalInterval()
			c.logger.Debug("scheduling DHCP renewal", "interface", c.iface, "in", renewalInterval)

			renewalTimer := time.NewTimer(renewalInterval)
			select {
			case <-ctx.Done():
				renewalTimer.Stop()
				c.logger.Info("DHCP client context cancelled", "interface", c.iface)
				return
			case <-renewalTimer.C:
			}

			newIP, newMask, err := c.dhcpRenew(ctx)
			if err != nil {
				c.logger.Warn("DHCP renewal failed, will re-discover", "interface", c.iface, "error", err)
				break
			}

			c.mu.Lock()
			renewalChanged := c.ip == nil || !c.ip.Equal(newIP)
			c.ip = newIP
			c.mask = newMask
			c.mu.Unlock()

			if renewalChanged && c.onChange != nil {
				c.logger.Info("IP changed after renewal", "interface", c.iface, "new_ip", newIP)
				c.onChange(c.iface, newIP, newMask)
			}
		}
	}
}

func (c *realClient) dhcpRequest(ctx context.Context) (net.IP, net.IPMask, error) {
	c.logger.Info("sending DHCP discover/request", "interface", c.iface, "mac", c.mac)

	conn, err := newDHCPConn(c.iface, 30*time.Second, 3)
	if err != nil {
		c.logger.Error("failed to create DHCP client", "interface", c.iface, "error", err)
		return nil, nil, err
	}
	defer conn.Close()

	resp, err := conn.Request(ctx)
	if err != nil {
		c.logger.Error("DHCP request failed", "interface", c.iface, "error", err)
		return nil, nil, err
	}

	if resp.ACK == nil {
		return nil, nil, fmt.Errorf("DHCP request succeeded but ACK is nil")
	}

	ip := resp.ACK.YourIPAddr
	mask := resp.ACK.SubnetMask()
	c.logger.Info("DHCP ACK received", "interface", c.iface, "ip", ip, "mask", mask)

	c.mu.Lock()
	c.lease = resp
	c.mu.Unlock()

	return ip, mask, nil
}

func (c *realClient) dhcpRenew(ctx context.Context) (net.IP, net.IPMask, error) {
	c.mu.RLock()
	lease := c.lease
	c.mu.RUnlock()

	if lease == nil {
		return nil, nil, fmt.Errorf("no active lease to renew")
	}

	c.logger.Info("sending DHCP renewal", "interface", c.iface)

	conn, err := newDHCPConn(c.iface, 30*time.Second, 1)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create DHCP client for renewal: %w", err)
	}
	defer conn.Close()

	resp, err := conn.Renew(ctx, lease)
	if err != nil {
		return nil, nil, fmt.Errorf("DHCP renewal failed: %w", err)
	}

	if resp.ACK == nil {
		return nil, nil, fmt.Errorf("DHCP renewal succeeded but ACK is nil")
	}

	ip := resp.ACK.YourIPAddr
	mask := resp.ACK.SubnetMask()
	c.logger.Info("DHCP renewal ACK received", "interface", c.iface, "ip", ip, "mask", mask)

	c.mu.Lock()
	c.lease = resp
	c.mu.Unlock()

	return ip, mask, nil
}

func (c *realClient) renewalInterval() time.Duration {
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

func (c *realClient) release() error {
	c.mu.RLock()
	ip := c.ip
	lease := c.lease
	c.mu.RUnlock()

	if ip == nil || lease == nil {
		c.logger.Debug("no active lease to release", "interface", c.iface)
		return nil
	}

	c.logger.Info("releasing DHCP lease", "interface", c.iface, "ip", ip)

	conn, err := newDHCPConn(c.iface, 10*time.Second, 1)
	if err != nil {
		return fmt.Errorf("failed to create DHCP client for release: %w", err)
	}
	defer conn.Close()

	if err := conn.Release(lease); err != nil {
		return fmt.Errorf("failed to send DHCP release: %w", err)
	}
	return nil
}

func newDHCPConn(iface string, timeout time.Duration, retries int) (*nclient4.Client, error) {
	opts := []nclient4.ClientOpt{nclient4.WithTimeout(timeout)}
	if retries > 1 {
		opts = append(opts, nclient4.WithRetry(retries))
	}
	return nclient4.New(iface, opts...)
}


