package geoip

import (
	"fmt"
	"net"
	"strings"

	"github.com/oschwald/maxminddb-golang"
)

type mmdbReader struct {
	db        *maxminddb.Reader
	byCountry map[string][]*net.IPNet
}

type countryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
}

// openMMDB opens the binary database and indexes every IPv4 network by country
// once at load time. Packet-time lookups are pure nftables set membership; this
// index only exists so RefreshGeoSets can materialise per-country CIDR lists.
func openMMDB(path string) (*mmdbReader, error) {
	db, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}

	r := &mmdbReader{db: db, byCountry: make(map[string][]*net.IPNet)}
	networks := db.Networks(maxminddb.SkipAliasedNetworks)
	var rec countryRecord
	for networks.Next() {
		subnet, err := networks.Network(&rec)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("iterate networks: %w", err)
		}
		if subnet.IP.To4() == nil {
			continue
		}
		cc := strings.ToUpper(strings.TrimSpace(rec.Country.ISOCode))
		if !isoAlpha2RE.MatchString(cc) {
			continue
		}
		r.byCountry[cc] = append(r.byCountry[cc], subnet)
	}
	if err := networks.Err(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("iterate networks: %w", err)
	}
	return r, nil
}

func (r *mmdbReader) LookupCountry(ip net.IP) (string, bool) {
	var rec countryRecord
	if err := r.db.Lookup(ip, &rec); err != nil {
		return "", false
	}
	cc := strings.ToUpper(strings.TrimSpace(rec.Country.ISOCode))
	if !isoAlpha2RE.MatchString(cc) {
		return "", false
	}
	return cc, true
}

func (r *mmdbReader) CountryCIDRs(cc string) ([]*net.IPNet, error) {
	nets := r.byCountry[strings.ToUpper(cc)]
	out := make([]*net.IPNet, len(nets))
	copy(out, nets)
	return out, nil
}

func (r *mmdbReader) Close() error { return r.db.Close() }
