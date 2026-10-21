package geoip

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
)

var isoAlpha2RE = regexp.MustCompile(`^[A-Z]{2}$`)

type tsvEntry struct {
	network *net.IPNet
	ones    int
	cc      string
}

// tsvReader is the deterministic, e2e-only replacement for the binary MMDB.
// Each row maps a CIDR to an ISO-3166 alpha-2 country code.
type tsvReader struct {
	entries   []tsvEntry // sorted by prefix length, longest first
	byCountry map[string][]*net.IPNet
}

func openTSV(path string) (*tsvReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := &tsvReader{byCountry: make(map[string][]*net.IPNet)}
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: expected \"<cidr>\\t<iso>\", got %q", lineNo, line)
		}
		_, network, err := net.ParseCIDR(fields[0])
		if err != nil {
			return nil, fmt.Errorf("line %d: invalid CIDR %q: %w", lineNo, fields[0], err)
		}
		cc := strings.ToUpper(fields[1])
		if !isoAlpha2RE.MatchString(cc) {
			return nil, fmt.Errorf("line %d: invalid ISO-3166 alpha-2 code %q", lineNo, fields[1])
		}
		if network.IP.To4() == nil {
			return nil, fmt.Errorf("line %d: only IPv4 CIDRs are supported, got %q", lineNo, fields[0])
		}
		ones, _ := network.Mask.Size()
		r.entries = append(r.entries, tsvEntry{network: network, ones: ones, cc: cc})
		r.byCountry[cc] = append(r.byCountry[cc], network)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(r.entries) == 0 {
		return nil, fmt.Errorf("no CIDR rows in %s", path)
	}

	// Longest-prefix match first.
	sort.SliceStable(r.entries, func(i, j int) bool {
		return r.entries[i].ones > r.entries[j].ones
	})
	return r, nil
}

func (r *tsvReader) LookupCountry(ip net.IP) (string, bool) {
	ip4 := ip.To4()
	if ip4 == nil {
		return "", false
	}
	for _, e := range r.entries {
		if e.network.Contains(ip4) {
			return e.cc, true
		}
	}
	return "", false
}

func (r *tsvReader) CountryCIDRs(cc string) ([]*net.IPNet, error) {
	nets := r.byCountry[strings.ToUpper(cc)]
	out := make([]*net.IPNet, len(nets))
	copy(out, nets)
	return out, nil
}

func (r *tsvReader) Close() error { return nil }
