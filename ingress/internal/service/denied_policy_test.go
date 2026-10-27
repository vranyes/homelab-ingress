package service

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"

	routerv1 "github.com/vranyes/homelab-ingress/ingress/pkg/api/router/v1"
)

// -- denied-cidrs tests --

func TestCreateLoadBalancer_StoresDeniedCIDRs(t *testing.T) {
	srv, st := newGeoTestServer(t, true)
	_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
		ServiceKey: "ns/svc",
		Ports:      validPorts(),
		// Host bits are canonicalised; duplicates dropped.
		DeniedCidrs: []string{" 203.0.113.1/24 ", "198.51.100.0/24", "203.0.113.0/24"},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, ok := st.GetService("ns/svc")
	if !ok {
		t.Fatal("service not stored")
	}
	if len(svc.DeniedCIDRs) != 2 || svc.DeniedCIDRs[0] != "203.0.113.0/24" || svc.DeniedCIDRs[1] != "198.51.100.0/24" {
		t.Fatalf("stored denied cidrs = %v", svc.DeniedCIDRs)
	}
}

func TestInvalidDeniedCIDRRejected(t *testing.T) {
	srv, st := newGeoTestServer(t, true)
	for _, denied := range [][]string{{"not-a-cidr"}, {"2001:db8::/32"}, {"203.0.113.0/24", "bogus"}} {
		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "ns/svc", Ports: validPorts(), DeniedCidrs: denied,
		})
		if codeOf(err) != codes.InvalidArgument {
			t.Fatalf("denied %v: error = %v, want InvalidArgument", denied, err)
		}
		if st.HasService("ns/svc") {
			t.Fatalf("denied %v: state was mutated on a rejected create", denied)
		}
	}
}

func TestUpdateLoadBalancer_ReplacesDeniedCIDRs(t *testing.T) {
	srv, st := newGeoTestServer(t, true)
	if _, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
		ServiceKey: "ns/svc", Ports: validPorts(), DeniedCidrs: []string{"203.0.113.0/24"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := srv.UpdateLoadBalancer(context.Background(), &routerv1.UpdateLoadBalancerRequest{
		ServiceKey: "ns/svc", Ports: validPorts(), DeniedCidrs: []string{"198.51.100.0/24"},
	}); err != nil {
		t.Fatal(err)
	}
	svc, _ := st.GetService("ns/svc")
	if len(svc.DeniedCIDRs) != 1 || svc.DeniedCIDRs[0] != "198.51.100.0/24" {
		t.Fatalf("updated denied cidrs = %v", svc.DeniedCIDRs)
	}

	// Empty list clears the deny-list.
	if _, err := srv.UpdateLoadBalancer(context.Background(), &routerv1.UpdateLoadBalancerRequest{
		ServiceKey: "ns/svc", Ports: validPorts(),
	}); err != nil {
		t.Fatal(err)
	}
	svc, _ = st.GetService("ns/svc")
	if len(svc.DeniedCIDRs) != 0 {
		t.Fatalf("denied cidrs not cleared: %v", svc.DeniedCIDRs)
	}
}

func TestGetAndList_ReturnDeniedCIDRs(t *testing.T) {
	srv, _ := newGeoTestServer(t, true)
	if _, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
		ServiceKey: "ns/svc", Ports: validPorts(),
		AllowedCidrs: []string{"192.168.1.0/24"}, DeniedCidrs: []string{"203.0.113.0/24"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := srv.GetLoadBalancer(context.Background(), &routerv1.GetLoadBalancerRequest{ServiceKey: "ns/svc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.DeniedCidrs) != 1 || got.DeniedCidrs[0] != "203.0.113.0/24" {
		t.Fatalf("Get denied cidrs = %v", got.DeniedCidrs)
	}

	list, err := srv.ListLoadBalancers(context.Background(), &routerv1.ListLoadBalancersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.LoadBalancers) != 1 || len(list.LoadBalancers[0].DeniedCidrs) != 1 {
		t.Fatalf("List denied cidrs = %v", list.LoadBalancers)
	}
}

// -- lb-policy tests --

func TestCreateLoadBalancer_StoresLBPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy string
		want   string
	}{
		{"empty default", "", ""},
		{"round-robin", "round-robin", "round-robin"},
		{"hash-source-ip", "hash-source-ip", "hash-source-ip"},
		{"random", "random", "random"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st := newGeoTestServer(t, true)
			_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
				ServiceKey: "ns/svc", Ports: validPorts(), LbPolicy: tc.policy,
			})
			if err != nil {
				t.Fatal(err)
			}
			svc, _ := st.GetService("ns/svc")
			if svc.LBPolicy != tc.want {
				t.Fatalf("stored policy = %q, want %q", svc.LBPolicy, tc.want)
			}
			got, err := srv.GetLoadBalancer(context.Background(), &routerv1.GetLoadBalancerRequest{ServiceKey: "ns/svc"})
			if err != nil {
				t.Fatal(err)
			}
			if got.LbPolicy != tc.want {
				t.Fatalf("Get policy = %q, want %q", got.LbPolicy, tc.want)
			}
		})
	}
}

func TestInvalidLBPolicyRejected(t *testing.T) {
	srv, st := newGeoTestServer(t, true)
	for _, policy := range []string{"least-conn", "round robin", "weighted"} {
		_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
			ServiceKey: "ns/svc", Ports: validPorts(), LbPolicy: policy,
		})
		if codeOf(err) != codes.InvalidArgument {
			t.Fatalf("policy %q: error = %v, want InvalidArgument", policy, err)
		}
		if st.HasService("ns/svc") {
			t.Fatalf("policy %q: state was mutated on a rejected create", policy)
		}
	}
}

func TestCreateLoadBalancer_LBPolicyCaseInsensitive(t *testing.T) {
	srv, st := newGeoTestServer(t, true)
	_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
		ServiceKey: "ns/svc", Ports: validPorts(), LbPolicy: " Random ",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := st.GetService("ns/svc")
	if svc.LBPolicy != "random" {
		t.Fatalf("stored policy = %q, want %q", svc.LBPolicy, "random")
	}
}

func TestUpdateLoadBalancer_ReplacesLBPolicy(t *testing.T) {
	srv, st := newGeoTestServer(t, true)
	if _, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
		ServiceKey: "ns/svc", Ports: validPorts(), LbPolicy: "random",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := srv.UpdateLoadBalancer(context.Background(), &routerv1.UpdateLoadBalancerRequest{
		ServiceKey: "ns/svc", Ports: validPorts(), LbPolicy: "hash-source-ip",
	}); err != nil {
		t.Fatal(err)
	}
	svc, _ := st.GetService("ns/svc")
	if svc.LBPolicy != "hash-source-ip" {
		t.Fatalf("updated policy = %q", svc.LBPolicy)
	}

	// A rejected update leaves the stored policy untouched.
	if _, err := srv.UpdateLoadBalancer(context.Background(), &routerv1.UpdateLoadBalancerRequest{
		ServiceKey: "ns/svc", Ports: validPorts(), LbPolicy: "least-conn",
	}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("error = %v, want InvalidArgument", err)
	}
	svc, _ = st.GetService("ns/svc")
	if svc.LBPolicy != "hash-source-ip" {
		t.Fatalf("state was mutated on a rejected update: %q", svc.LBPolicy)
	}
}
