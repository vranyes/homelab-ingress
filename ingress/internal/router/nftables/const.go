package nftables

const (
	CtStatusSNAT  uint32 = 1 << 4 // IPS_SRC_NAT — connection needs src nat
	CtStatusDNAT  uint32 = 1 << 5 // IPS_DST_NAT — connection needs dst nat
	tableName            = "homelab_ingress"
	geoSetPrefix         = "geo_"
	denySetPrefix        = "deny_"
	priority             = -110 // Priority -110: runs before inet fw4's dstnat chain (-100).
)
