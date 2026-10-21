package controller

import (
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	AnnotationPrefix      = "homelab-ingress.vranyes.com"
	AnnotationSkip        = AnnotationPrefix + "/skip"
	AnnotationAllowedGeos = AnnotationPrefix + "/allowed-geos"
	Finalizer             = AnnotationPrefix + "/cleanup"
)

var isoAlpha2RE = regexp.MustCompile(`^[A-Z]{2}$`)

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
