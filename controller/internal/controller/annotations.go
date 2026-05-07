package controller

import (
	corev1 "k8s.io/api/core/v1"
)

const (
	AnnotationPrefix = "homelab-ingress.vranyes.com"
	AnnotationSkip   = AnnotationPrefix + "/skip"
	Finalizer        = AnnotationPrefix + "/cleanup"
)

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
