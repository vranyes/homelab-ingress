package nftables_test

import (
	"github.com/vranyes/homelab-ingress/ingress/internal/router/nftables"
	"github.com/vranyes/homelab-ingress/ingress/internal/state"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func makeVal(ip []byte, port int) []byte {
	v := make([]byte, 8)
	copy(v, ip)
	v[4] = byte(port >> 8)
	v[5] = byte(port)
	return v
}

var _ = Describe("DestinationsEqual", func() {
	type destEqualCase struct {
		name     string
		values   [][]byte
		dests    []state.LBDestination
		expected bool
	}

	destEqualCases := func() []destEqualCase {
		ipA := []byte{10, 42, 0, 29}
		ipB := []byte{10, 42, 0, 135}
		ipC := []byte{10, 42, 0, 1}

		valA := makeVal(ipA, 8000)
		valB := makeVal(ipB, 8000)
		valC := makeVal(ipC, 8443)

		return []destEqualCase{
			{name: "same order", values: [][]byte{valA, valB}, dests: []state.LBDestination{{Address: ipA, Port: 8000}, {Address: ipB, Port: 8000}}, expected: true},
			{name: "different order", values: [][]byte{valB, valA}, dests: []state.LBDestination{{Address: ipA, Port: 8000}, {Address: ipB, Port: 8000}}, expected: true},
			{name: "three destinations any order", values: [][]byte{valC, valA, valB}, dests: []state.LBDestination{{Address: ipA, Port: 8000}, {Address: ipB, Port: 8000}, {Address: ipC, Port: 8443}}, expected: true},
			{name: "different IP", values: [][]byte{valA, valC}, dests: []state.LBDestination{{Address: ipA, Port: 8000}, {Address: ipB, Port: 8000}}, expected: false},
			{name: "mismatched lengths", values: [][]byte{valA}, dests: []state.LBDestination{{Address: ipA, Port: 8000}, {Address: ipB, Port: 8000}}, expected: false},
			{name: "empty both", values: [][]byte{}, dests: []state.LBDestination{}, expected: true},
			{name: "single destination", values: [][]byte{valA}, dests: []state.LBDestination{{Address: ipA, Port: 8000}}, expected: true},
			{name: "same IP different port", values: [][]byte{valA}, dests: []state.LBDestination{{Address: ipA, Port: 8443}}, expected: false},
			{name: "duplicate values (should still match)", values: [][]byte{valA, valA}, dests: []state.LBDestination{{Address: ipA, Port: 8000}, {Address: ipA, Port: 8000}}, expected: true},
			{name: "port zero", values: [][]byte{{10, 42, 0, 29, 0x00, 0x00, 0x00, 0x00}}, dests: []state.LBDestination{{Address: ipA, Port: 0}}, expected: true},
		}
	}()

	DescribeTable("returns expected result",
		func(values [][]byte, dests []state.LBDestination, expected bool) {
			Expect(nftables.DestinationsEqual(values, dests)).To(Equal(expected))
		},
		func() []TableEntry {
			var entries []TableEntry
			for _, tc := range destEqualCases {
				entries = append(entries, Entry(tc.name, tc.values, tc.dests, tc.expected))
			}
			return entries
		}(),
	)
})

var _ = Describe("ChainName", func() {
	type chainNameCase struct {
		prefix     string
		serviceKey string
		expected   string
	}

	chainNameCases := []chainNameCase{
		{prefix: "ingress", serviceKey: "traefik/traefik", expected: "ingress_traefik_traefik"},
		{prefix: "forward", serviceKey: "traefik/traefik", expected: "forward_traefik_traefik"},
		{prefix: "ingress", serviceKey: "my-service", expected: "ingress_my_service"},
		{prefix: "ingress", serviceKey: "a/b/c", expected: "ingress_a_b_c"},
	}

	DescribeTable("returns expected chain name",
		func(prefix, serviceKey, expected string) {
			Expect(nftables.ChainName(prefix, serviceKey)).To(Equal(expected))
		},
		func() []TableEntry {
			var entries []TableEntry
			for _, tc := range chainNameCases {
				entries = append(entries, Entry(tc.expected, tc.prefix, tc.serviceKey, tc.expected))
			}
			return entries
		}(),
	)

	It("truncates to maximum 32 characters", func() {
		got := nftables.ChainName("ingress", "very-long-service-name-that-exceeds-thirty-two-chars")
		Expect(len(got)).To(BeNumerically("<=", 32))
	})
})
