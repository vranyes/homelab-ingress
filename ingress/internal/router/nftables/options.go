package nftables

import "net"

// GeoLookup resolves an ISO-3166 alpha-2 country code to its CIDRs. The bool
// is false when the code has no known networks.
type GeoLookup func(cc string) ([]*net.IPNet, bool)

type MasqueradeRule struct {
	InputDevice  string
	OutputDevice string
}

type options struct {
	masqueradeRules []MasqueradeRule
	geoLookup       GeoLookup
	healthChecker   HealthChecker
}

type Option func(*options)

func WithMasquerade(inputDev, outputDev string) Option {
	return func(o *options) {
		o.masqueradeRules = append(o.masqueradeRules, MasqueradeRule{
			InputDevice:  inputDev,
			OutputDevice: outputDev,
		})
	}
}

// WithGeoLookup supplies the country→CIDR resolver used by AddRules.
func WithGeoLookup(lookup GeoLookup) Option {
	return func(o *options) {
		o.geoLookup = lookup
	}
}

// HealthChecker reports whether a backend is currently considered healthy.
// The nftables manager consults it at rule-build time to exclude down
// backends from the DNAT maps. A nil checker (the default) disables
// filtering. The health package's Checker implements this interface.
type HealthChecker interface {
	IsHealthy(serviceKey, addr string, port uint16) bool
}

// WithHealthChecker supplies the backend-health source used by AddRules.
// Filtering is in-memory at rule-build time only; persisted State is never
// mutated.
func WithHealthChecker(hc HealthChecker) Option {
	return func(o *options) {
		o.healthChecker = hc
	}
}
