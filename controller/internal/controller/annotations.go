package controller

import (
	"fmt"
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	AnnotationPrefix      = "homelab-ingress.vranyes.com"
	AnnotationSkip        = AnnotationPrefix + "/skip"
	AnnotationAllowedGeos = AnnotationPrefix + "/allowed-geos"
	// AnnotationAllowedCIDRs is a comma-separated list of IPv4 CIDRs that are
	// always allowed alongside the allowed-geos country list (e.g. LAN ranges
	// such as 192.168.1.0/24, which have no country in the geo database).
	AnnotationAllowedCIDRs = AnnotationPrefix + "/allowed-cidrs"
	// AnnotationRateLimit is the sustained number of new connections per
	// second allowed from a single source IP. Absent disables rate limiting.
	AnnotationRateLimit = AnnotationPrefix + "/rate-limit"
	// AnnotationRateLimitBurst is the token-bucket capacity. When absent it
	// defaults to the sustained rate.
	AnnotationRateLimitBurst = AnnotationPrefix + "/rate-limit-burst"
	Finalizer                = AnnotationPrefix + "/cleanup"
)

// RateLimit is the normalised form of the rate-limit annotations.
type RateLimit struct {
	Rate  uint64
	Burst uint32
}

var isoAlpha2RE = regexp.MustCompile(`^[A-Z]{2}$`)

// RateLimitFromAnnotations parses and normalises the rate-limit annotations.
// A missing rate annotation yields (nil, nil): rate limiting is disabled.
// Invalid values return an error so the reconciler can keep the last known
// good limit instead of silently changing protection.
func RateLimitFromAnnotations(svc *corev1.Service) (*RateLimit, error) {
	if svc == nil || svc.Annotations == nil {
		return nil, nil
	}
	rawRate, ok := svc.Annotations[AnnotationRateLimit]
	if !ok || strings.TrimSpace(rawRate) == "" {
		return nil, nil
	}
	rate, err := parsePositiveUint(rawRate, 64)
	if err != nil || rate == 0 {
		return nil, fmt.Errorf("invalid %s %q: want a positive integer (new connections per second)", AnnotationRateLimit, rawRate)
	}

	burst := rate
	if rawBurst, ok := svc.Annotations[AnnotationRateLimitBurst]; ok && strings.TrimSpace(rawBurst) != "" {
		burst, err = parsePositiveUint(rawBurst, 32)
		if err != nil || burst == 0 {
			return nil, fmt.Errorf("invalid %s %q: want a positive integer", AnnotationRateLimitBurst, rawBurst)
		}
	} else if burst > math.MaxUint32 {
		burst = math.MaxUint32
	}

	return &RateLimit{Rate: rate, Burst: uint32(burst)}, nil
}

// parsePositiveUint parses a base-10 unsigned integer of at most bitSize bits.
func parsePositiveUint(raw string, bitSize int) (uint64, error) {
	return strconv.ParseUint(strings.TrimSpace(raw), 10, bitSize)
}

// rateLimitChanged compares the normalised forms. Callers must normalise both
// sides first; the controller and the ingress share the same canonical form.
func rateLimitChanged(existing, desired *RateLimit) bool {
	if existing == nil || desired == nil {
		return existing != desired
	}
	return existing.Rate != desired.Rate || existing.Burst != desired.Burst
}

// AllowedGeos parses the allowed-geos annotation into normalised ISO-3166
// alpha-2 country codes. Invalid or empty entries are dropped; a missing
// annotation yields nil (allow all traffic).
func AllowedGeos(svc *corev1.Service) []string {
	if svc == nil || svc.Annotations == nil {
		return nil
	}
	raw := svc.Annotations[AnnotationAllowedGeos]
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := make([]string, 0, 2)
	seen := make(map[string]struct{}, 2)
	for _, part := range strings.Split(raw, ",") {
		cc := strings.ToUpper(strings.TrimSpace(part))
		if !isoAlpha2RE.MatchString(cc) {
			continue
		}
		if _, dup := seen[cc]; dup {
			continue
		}
		seen[cc] = struct{}{}
		out = append(out, cc)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AllowedCIDRs parses the allowed-cidrs annotation into normalised IPv4 CIDR
// strings. Entries are trimmed, canonicalised via IPNet.String(), and
// de-duplicated. A missing or empty annotation yields (nil, nil): no static
// CIDR allow-list. Any invalid entry (unparseable CIDR or IPv6) returns an
// error so the reconciler keeps the last-good CIDRs instead of silently
// allowing all traffic.
func AllowedCIDRs(svc *corev1.Service) ([]string, error) {
	if svc == nil || svc.Annotations == nil {
		return nil, nil
	}
	raw := svc.Annotations[AnnotationAllowedCIDRs]
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	seen := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		_, ipNet, err := net.ParseCIDR(trimmed)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q: %q is not a valid CIDR", AnnotationAllowedCIDRs, raw, trimmed)
		}
		if ipNet.IP.To4() == nil {
			return nil, fmt.Errorf("invalid %s %q: %q is not an IPv4 CIDR", AnnotationAllowedCIDRs, raw, trimmed)
		}
		canonical := ipNet.String()
		if _, dup := seen[canonical]; dup {
			continue
		}
		seen[canonical] = struct{}{}
		out = append(out, canonical)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func ShouldSkip(svc *corev1.Service) bool {
	if svc.Annotations == nil {
		return false
	}
	return svc.Annotations[AnnotationSkip] == "true"
}

func HasFinalizer(svc *corev1.Service) bool {
	for _, f := range svc.Finalizers {
		if f == Finalizer {
			return true
		}
	}
	return false
}

func AddFinalizer(svc *corev1.Service) {
	if !HasFinalizer(svc) {
		svc.Finalizers = append(svc.Finalizers, Finalizer)
	}
}

func RemoveFinalizer(svc *corev1.Service) {
	var result []string
	for _, f := range svc.Finalizers {
		if f == Finalizer {
			continue
		}
		result = append(result, f)
	}
	svc.Finalizers = result
}
