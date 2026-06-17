//go:build linux

package nftables_test

import (
	"io"
	"log/slog"
	"net"

	go_nftables "github.com/google/nftables"
	"github.com/mdlayher/netlink"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/sys/unix"

	"github.com/vranyes/homelab-ingress/ingress/internal/router/nftables"
	"github.com/vranyes/homelab-ingress/ingress/internal/state"
)

func nftMsgType(subtype uint8) netlink.HeaderType {
	return netlink.HeaderType((uint16(unix.NFNL_SUBSYS_NFTABLES) << 8) | uint16(subtype))
}

var (
	getTableType   = nftMsgType(unix.NFT_MSG_GETTABLE)
	getChainType   = nftMsgType(unix.NFT_MSG_GETCHAIN)
	getSetType     = nftMsgType(unix.NFT_MSG_GETSET)
	getSetElemType = nftMsgType(unix.NFT_MSG_GETSETELEM)
	getRuleType    = nftMsgType(unix.NFT_MSG_GETRULE)
)

func testDial(req []netlink.Message) ([]netlink.Message, error) {
	if len(req) == 0 {
		return nil, io.EOF
	}

	t := req[0].Header.Type
	switch t {
	case getTableType:
		return tableResponse(req[0]), nil
	case getChainType:
		return chainResponse(req[0]), nil
	case getSetType, getSetElemType, getRuleType:
		return nil, io.EOF
	default:
		return req, nil
	}
}

func tableResponse(msg netlink.Message) []netlink.Message {
	family := byte(go_nftables.TableFamilyIPv4)
	if len(msg.Data) > 0 {
		family = msg.Data[0]
	}
	data, _ := netlink.MarshalAttributes([]netlink.Attribute{
		{Type: unix.NFTA_TABLE_NAME, Data: []byte("dummy\x00")},
		{Type: unix.NFTA_TABLE_FLAGS, Data: []byte{0, 0, 0, 0}},
	})
	return []netlink.Message{{
		Header: netlink.Header{
			Type: netlink.HeaderType((uint16(unix.NFNL_SUBSYS_NFTABLES) << 8) | uint16(unix.NFT_MSG_NEWTABLE)),
		},
		Data: append([]byte{family, 0, 0, 0}, data...),
	}}
}

func chainResponse(msg netlink.Message) []netlink.Message {
	family := byte(go_nftables.TableFamilyIPv4)
	if len(msg.Data) > 0 {
		family = msg.Data[0]
	}
	data, _ := netlink.MarshalAttributes([]netlink.Attribute{
		{Type: unix.NFTA_CHAIN_TABLE, Data: []byte("other\x00")},
		{Type: unix.NFTA_CHAIN_NAME, Data: []byte("other\x00")},
	})
	return []netlink.Message{{
		Header: netlink.Header{
			Type: netlink.HeaderType((uint16(unix.NFNL_SUBSYS_NFTABLES) << 8) | uint16(unix.NFT_MSG_NEWCHAIN)),
		},
		Data: append([]byte{family, 0, 0, 0}, data...),
	}}
}

var _ = Describe("AddRules", func() {
	var (
		c      *go_nftables.Conn
		logger *slog.Logger
		m      *nftables.Manager
	)

	BeforeEach(func() {
		var err error
		c, err = go_nftables.New(go_nftables.WithTestDial(testDial))
		Expect(err).NotTo(HaveOccurred())
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		m = nftables.NewManagerWithConn(c, logger)
	})

	Context("single destination", func() {
		var svc state.LBService

		BeforeEach(func() {
			svc = state.LBService{
				ServiceName:   "test/app",
				SourceAddress: net.IP{10, 0, 0, 1},
				Targets: []state.LBTarget{{
					Protocol: "tcp",
					SrcPort:  80,
					Dst: []state.LBDestination{
						{Address: net.IP{10, 42, 0, 29}, Port: 8000},
					},
				}},
			}
		})

		It("succeeds", func() {
			Expect(m.AddRules(svc)).To(Succeed())
		})
	})

	Context("multiple destinations", func() {
		var svc state.LBService

		BeforeEach(func() {
			svc = state.LBService{
				ServiceName:   "test/app",
				SourceAddress: net.IP{10, 0, 0, 1},
				Targets: []state.LBTarget{{
					Protocol: "tcp",
					SrcPort:  80,
					Dst: []state.LBDestination{
						{Address: net.IP{10, 42, 0, 29}, Port: 8000},
						{Address: net.IP{10, 42, 0, 135}, Port: 8000},
					},
				}},
			}
		})

		It("succeeds with LB map", func() {
			Expect(m.AddRules(svc)).To(Succeed())
		})
	})
})
