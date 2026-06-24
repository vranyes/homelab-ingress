//go:build linux

package nftables

import (
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

func newTestConn(t *testing.T) *nftables.Conn {
	t.Helper()
	nextHandle := uint64(1)
	c, err := nftables.New(nftables.WithTestDial(
		func(req []netlink.Message) ([]netlink.Message, error) {
			var replies []netlink.Message
			for _, msg := range req {
				if msg.Header.Flags&netlink.Echo != 0 {
					data := append([]byte{}, msg.Data...)
					if msg.Header.Type == netlink.HeaderType(
						(unix.NFNL_SUBSYS_NFTABLES<<8)|unix.NFT_MSG_NEWRULE,
					) {
						attrs, _ := netlink.MarshalAttributes([]netlink.Attribute{
							{Type: unix.NFTA_RULE_HANDLE,
								Data: binaryutil.BigEndian.PutUint64(nextHandle)},
						})
						nextHandle++
						data = append(data, attrs...)
					}
					replies = append(replies, netlink.Message{
						Header: msg.Header,
						Data:   data,
					})
				}
			}
			return replies, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
