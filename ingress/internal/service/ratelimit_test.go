package service

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"

	routerv1 "github.com/vranyes/homelab-ingress/ingress/pkg/api/router/v1"
)

func TestCreateLoadBalancer_RateLimit(t *testing.T) {
	ctx := context.Background()

	t.Run("stores an explicit limit", func(t *testing.T) {
		srv, st := testServer()
		_, err := srv.CreateLoadBalancer(ctx, &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "ns/svc",
			Ports:      validPorts(),
			RateLimit:  &routerv1.RateLimit{Rate: 5, Burst: 10},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		svc, ok := st.GetService("ns/svc")
		if !ok || svc.RateLimit == nil {
			t.Fatalf("rate limit not stored: %+v", svc)
		}
		if svc.RateLimit.Rate != 5 || svc.RateLimit.Burst != 10 {
			t.Fatalf("stored rate limit = %+v", svc.RateLimit)
		}
	})

	t.Run("zero burst defaults to the rate", func(t *testing.T) {
		srv, _ := testServer()
		_, err := srv.CreateLoadBalancer(ctx, &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "ns/svc",
			Ports:      validPorts(),
			RateLimit:  &routerv1.RateLimit{Rate: 5},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		resp, err := srv.GetLoadBalancer(ctx, &routerv1.GetLoadBalancerRequest{ServiceKey: "ns/svc"})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if resp.RateLimit == nil || resp.RateLimit.Rate != 5 || resp.RateLimit.Burst != 5 {
			t.Fatalf("returned rate limit = %+v", resp.RateLimit)
		}
	})

	t.Run("zero rate is rejected", func(t *testing.T) {
		srv, st := testServer()
		_, err := srv.CreateLoadBalancer(ctx, &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "ns/svc",
			Ports:      validPorts(),
			RateLimit:  &routerv1.RateLimit{Rate: 0, Burst: 5},
		})
		if codeOf(err) != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument, got %v", err)
		}
		if st.HasService("ns/svc") {
			t.Fatal("service must not be created on an invalid limit")
		}
	})

	t.Run("absent limit is nil", func(t *testing.T) {
		srv, st := testServer()
		if _, err := srv.CreateLoadBalancer(ctx, &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "ns/svc",
			Ports:      validPorts(),
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
		svc, _ := st.GetService("ns/svc")
		if svc.RateLimit != nil {
			t.Fatalf("expected nil rate limit, got %+v", svc.RateLimit)
		}
	})
}

func TestUpdateLoadBalancer_RateLimit(t *testing.T) {
	ctx := context.Background()
	srv, st := testServer()
	if _, err := srv.CreateLoadBalancer(ctx, &routerv1.CreateLoadBalancerRequest{
		ServiceKey: "ns/svc",
		Ports:      validPorts(),
		RateLimit:  &routerv1.RateLimit{Rate: 5, Burst: 5},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Run("replaces the limit", func(t *testing.T) {
		if _, err := srv.UpdateLoadBalancer(ctx, &routerv1.UpdateLoadBalancerRequest{
			ServiceKey: "ns/svc",
			Ports:      validPorts(),
			RateLimit:  &routerv1.RateLimit{Rate: 7, Burst: 9},
		}); err != nil {
			t.Fatalf("update: %v", err)
		}
		svc, _ := st.GetService("ns/svc")
		if svc.RateLimit == nil || svc.RateLimit.Rate != 7 || svc.RateLimit.Burst != 9 {
			t.Fatalf("updated rate limit = %+v", svc.RateLimit)
		}
	})

	t.Run("rejects a zero rate without changing state", func(t *testing.T) {
		_, err := srv.UpdateLoadBalancer(ctx, &routerv1.UpdateLoadBalancerRequest{
			ServiceKey: "ns/svc",
			Ports:      validPorts(),
			RateLimit:  &routerv1.RateLimit{Rate: 0, Burst: 9},
		})
		if codeOf(err) != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument, got %v", err)
		}
		svc, _ := st.GetService("ns/svc")
		if svc.RateLimit == nil || svc.RateLimit.Rate != 7 {
			t.Fatalf("invalid update changed state: %+v", svc.RateLimit)
		}
	})

	t.Run("clears the limit when absent", func(t *testing.T) {
		if _, err := srv.UpdateLoadBalancer(ctx, &routerv1.UpdateLoadBalancerRequest{
			ServiceKey: "ns/svc",
			Ports:      validPorts(),
		}); err != nil {
			t.Fatalf("update: %v", err)
		}
		svc, _ := st.GetService("ns/svc")
		if svc.RateLimit != nil {
			t.Fatalf("expected nil rate limit after clearing, got %+v", svc.RateLimit)
		}
	})
}
