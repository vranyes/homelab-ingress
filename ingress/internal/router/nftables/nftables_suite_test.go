package nftables_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestNftables(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Nftables Suite")
}
