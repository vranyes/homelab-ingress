package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestShouldSkip(t *testing.T) {
	tests := []struct {
		name   string
		svc    *corev1.Service
		expect bool
	}{
		{
			name:   "nil annotations",
			svc:    &corev1.Service{},
			expect: false,
		},
		{
			name:   "skip annotation not set",
			svc:    &corev1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}},
			expect: false,
		},
		{
			name:   "skip annotation false",
			svc:    &corev1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationSkip: "false"}}},
			expect: false,
		},
		{
			name:   "skip annotation true",
			svc:    &corev1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationSkip: "true"}}},
			expect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldSkip(tt.svc); got != tt.expect {
				t.Errorf("expected %v, got %v", tt.expect, got)
			}
		})
	}
}

func TestHasFinalizer(t *testing.T) {
	tests := []struct {
		name   string
		svc    *corev1.Service
		expect bool
	}{
		{
			name:   "no finalizers",
			svc:    &corev1.Service{},
			expect: false,
		},
		{
			name:   "has our finalizer",
			svc:    &corev1.Service{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{Finalizer}}},
			expect: true,
		},
		{
			name:   "has other finalizer",
			svc:    &corev1.Service{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{"kubernetes"}}},
			expect: false,
		},
		{
			name:   "has both finalizers",
			svc:    &corev1.Service{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{"kubernetes", Finalizer}}},
			expect: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasFinalizer(tt.svc); got != tt.expect {
				t.Errorf("expected %v, got %v", tt.expect, got)
			}
		})
	}
}

func TestAddFinalizer(t *testing.T) {
	t.Run("adds finalizer when not present", func(t *testing.T) {
		svc := &corev1.Service{}
		AddFinalizer(svc)
		if !HasFinalizer(svc) {
			t.Fatal("expected finalizer to be added")
		}
	})

	t.Run("does not duplicate finalizer", func(t *testing.T) {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{Finalizer}}}
		AddFinalizer(svc)
		if len(svc.Finalizers) != 1 {
			t.Errorf("expected 1 finalizer, got %d", len(svc.Finalizers))
		}
	})
}

func TestRemoveFinalizer(t *testing.T) {
	t.Run("removes our finalizer", func(t *testing.T) {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{Finalizer, "kubernetes"}}}
		RemoveFinalizer(svc)
		if HasFinalizer(svc) {
			t.Fatal("expected finalizer to be removed")
		}
		if len(svc.Finalizers) != 1 || svc.Finalizers[0] != "kubernetes" {
			t.Errorf("expected only kubernetes finalizer, got %v", svc.Finalizers)
		}
	})

	t.Run("does nothing when not present", func(t *testing.T) {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{"kubernetes"}}}
		RemoveFinalizer(svc)
		if len(svc.Finalizers) != 1 {
			t.Errorf("expected 1 finalizer, got %d", len(svc.Finalizers))
		}
	})
}
