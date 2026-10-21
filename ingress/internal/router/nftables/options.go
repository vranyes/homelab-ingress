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
