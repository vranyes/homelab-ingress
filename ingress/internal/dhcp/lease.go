package dhcp

import (
	"net"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
)

type LeaseInfo struct {
	ServiceName string        `json:"service_name"`
	Address     net.IP        `json:"address"`
	Mask        net.IPMask    `json:"mask"`
	ACKBytes    []byte        `json:"ack_bytes"`
	OfferBytes  []byte        `json:"offer_bytes"`
}

func newLeaseInfo(lease *nclient4.Lease, ip net.IP, mask net.IPMask) *LeaseInfo {
	if lease == nil || lease.ACK == nil {
		return nil
	}
	ackBytes := lease.ACK.ToBytes()
	var offerBytes []byte
	if lease.Offer != nil {
		offerBytes = lease.Offer.ToBytes()
	}
	return &LeaseInfo{
		Address:    ip,
		Mask:       mask,
		ACKBytes:   ackBytes,
		OfferBytes: offerBytes,
	}
}

func leaseFromInfo(info *LeaseInfo) *nclient4.Lease {
	if info == nil || len(info.ACKBytes) == 0 {
		return nil
	}
	ack, err := dhcpv4.FromBytes(info.ACKBytes)
	if err != nil {
		return nil
	}
	var offer *dhcpv4.DHCPv4
	if len(info.OfferBytes) > 0 {
		offer, err = dhcpv4.FromBytes(info.OfferBytes)
		if err != nil {
			offer = nil
		}
	}
	return &nclient4.Lease{
		Offer: offer,
		ACK:   ack,
	}
}
