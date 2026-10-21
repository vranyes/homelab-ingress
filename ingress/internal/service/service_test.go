package service

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/vranyes/homelab-ingress/ingress/internal/state"
	routerv1 "github.com/vranyes/homelab-ingress/ingress/pkg/api/router/v1"
)

func testServer() (*RouterServer, *state.State) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := state.NewState("eth0", nil, logger)
	return NewRouterServer(st, logger), st
}

func validPorts() []*routerv1.PortMapping {
	return []*routerv1.PortMapping{{
		Protocol: "tcp",
		SrcPort:  80,
		Dst:      []*routerv1.Destination{{Addr: "10.0.0.1", Port: 8080}},
	}}
}

func codeOf(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	s, ok := status.FromError(err)
	if !ok {
		return codes.Unknown
	}
	return s.Code()
}

func TestCreateLoadBalancer_Validation(t *testing.T) {
	ctx := context.Background()

	t.Run("empty service key rejected", func(t *testing.T) {
		srv, _ := testServer()
		_, err := srv.CreateLoadBalancer(ctx, &routerv1.CreateLoadBalancerRequest{ServiceKey: "", Ports: validPorts()})
		if codeOf(err) != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument, got %v", err)
		}
	})

	t.Run("no ports rejected", func(t *testing.T) {
		srv, _ := testServer()
		_, err := srv.CreateLoadBalancer(ctx, &routerv1.CreateLoadBalancerRequest{ServiceKey: "ns/svc"})
		if codeOf(err) != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument, got %v", err)
		}
	})

	t.Run("duplicate rejected", func(t *testing.T) {
		srv, _ := testServer()
		req := &routerv1.CreateLoadBalancerRequest{ServiceKey: "ns/svc", Ports: validPorts()}
		if _, err := srv.CreateLoadBalancer(ctx, req); err != nil {
			t.Fatalf("first create: %v", err)
		}
		_, err := srv.CreateLoadBalancer(ctx, req)
		if codeOf(err) != codes.AlreadyExists {
			t.Fatalf("expected AlreadyExists, got %v", err)
		}
	})
}

func TestGetLoadBalancer_BeforeDHCP(t *testing.T) {
	ctx := context.Background()
	srv, _ := testServer()
	if _, err := srv.CreateLoadBalancer(ctx, &routerv1.CreateLoadBalancerRequest{ServiceKey: "ns/svc", Ports: validPorts()}); err != nil {
		t.Fatalf("create: %v", err)
	}

	resp, err := srv.GetLoadBalancer(ctx, &routerv1.GetLoadBalancerRequest{ServiceKey: "ns/svc"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.AssignedIp != "" {
		t.Fatalf("expected empty AssignedIp before DHCP, got %q", resp.AssignedIp)
	}
	if resp.InterfaceName != "" {
		t.Fatalf("expected empty InterfaceName before macvlan, got %q", resp.InterfaceName)
	}

	t.Run("missing service", func(t *testing.T) {
		_, err := srv.GetLoadBalancer(ctx, &routerv1.GetLoadBalancerRequest{ServiceKey: "ns/nope"})
		if codeOf(err) != codes.NotFound {
			t.Fatalf("expected NotFound, got %v", err)
		}
	})

	t.Run("empty key", func(t *testing.T) {
		_, err := srv.GetLoadBalancer(ctx, &routerv1.GetLoadBalancerRequest{ServiceKey: ""})
		if codeOf(err) != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument, got %v", err)
		}
	})
}

// TestListLoadBalancers_BeforeDHCP is the regression test for the "<nil>"
// bug: List used to return assigned_ip "<nil>" (net.IP(nil).String()) for
// DHCP-pending services, which the controller's IP poller then wrote into
// Service status as a literal IP. List must agree with Get ("").
func TestListLoadBalancers_BeforeDHCP(t *testing.T) {
	ctx := context.Background()
	srv, _ := testServer()
	if _, err := srv.CreateLoadBalancer(ctx, &routerv1.CreateLoadBalancerRequest{ServiceKey: "ns/svc", Ports: validPorts()}); err != nil {
		t.Fatalf("create: %v", err)
	}

	list, err := srv.ListLoadBalancers(ctx, &routerv1.ListLoadBalancersRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.LoadBalancers) != 1 {
		t.Fatalf("expected 1 load balancer, got %d", len(list.LoadBalancers))
	}
	if got := list.LoadBalancers[0].AssignedIp; got != "" {
		t.Fatalf("expected empty AssignedIp before DHCP, got %q", got)
	}
}

func TestListLoadBalancers_AfterDHCP(t *testing.T) {
	ctx := context.Background()
	srv, st := testServer()
	if _, err := srv.CreateLoadBalancer(ctx, &routerv1.CreateLoadBalancerRequest{ServiceKey: "ns/svc", Ports: validPorts()}); err != nil {
		t.Fatalf("create: %v", err)
	}
	svc, ok := st.GetService("ns/svc")
	if !ok {
		t.Fatalf("service missing from state")
	}
	svc.SourceAddress = net.ParseIP("10.44.0.50")

	for _, tc := range []struct {
		name string
		call func() (string, error)
	}{
		{"list", func() (string, error) {
			list, err := srv.ListLoadBalancers(ctx, &routerv1.ListLoadBalancersRequest{})
			if err != nil || len(list.LoadBalancers) != 1 {
				return "", err
			}
			return list.LoadBalancers[0].AssignedIp, nil
		}},
		{"get", func() (string, error) {
			resp, err := srv.GetLoadBalancer(ctx, &routerv1.GetLoadBalancerRequest{ServiceKey: "ns/svc"})
			if err != nil {
				return "", err
			}
			return resp.AssignedIp, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ip, err := tc.call()
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if ip != "10.44.0.50" {
				t.Fatalf("expected 10.44.0.50, got %q", ip)
			}
		})
	}
}

func TestUpdateLoadBalancer(t *testing.T) {
	ctx := context.Background()
	srv, _ := testServer()

	t.Run("missing service", func(t *testing.T) {
		_, err := srv.UpdateLoadBalancer(ctx, &routerv1.UpdateLoadBalancerRequest{ServiceKey: "ns/nope", Ports: validPorts()})
		if codeOf(err) != codes.NotFound {
			t.Fatalf("expected NotFound, got %v", err)
		}
	})

	t.Run("empty key", func(t *testing.T) {
		_, err := srv.UpdateLoadBalancer(ctx, &routerv1.UpdateLoadBalancerRequest{ServiceKey: ""})
		if codeOf(err) != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument, got %v", err)
		}
	})

	t.Run("retargets destinations", func(t *testing.T) {
		if _, err := srv.CreateLoadBalancer(ctx, &routerv1.CreateLoadBalancerRequest{ServiceKey: "ns/svc", Ports: validPorts()}); err != nil {
			t.Fatalf("create: %v", err)
		}
		updated := []*routerv1.PortMapping{{
			Protocol: "tcp",
			SrcPort:  80,
			Dst: []*routerv1.Destination{
				{Addr: "10.0.0.1", Port: 8080},
				{Addr: "10.0.0.2", Port: 8080},
			},
		}}
		if _, err := srv.UpdateLoadBalancer(ctx, &routerv1.UpdateLoadBalancerRequest{ServiceKey: "ns/svc", Ports: updated}); err != nil {
			t.Fatalf("update: %v", err)
		}
		resp, err := srv.GetLoadBalancer(ctx, &routerv1.GetLoadBalancerRequest{ServiceKey: "ns/svc"})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(resp.Ports) != 1 || len(resp.Ports[0].Dst) != 2 {
			t.Fatalf("expected 2 destinations after update, got %+v", resp.Ports)
		}
	})
}

func TestDeleteLoadBalancer(t *testing.T) {
	ctx := context.Background()
	srv, _ := testServer()

	t.Run("empty key", func(t *testing.T) {
		_, err := srv.DeleteLoadBalancer(ctx, &routerv1.DeleteLoadBalancerRequest{ServiceKey: ""})
		if codeOf(err) != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument, got %v", err)
		}
	})

	t.Run("missing service is a no-op", func(t *testing.T) {
		if _, err := srv.DeleteLoadBalancer(ctx, &routerv1.DeleteLoadBalancerRequest{ServiceKey: "ns/nope"}); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("deletes created service", func(t *testing.T) {
		if _, err := srv.CreateLoadBalancer(ctx, &routerv1.CreateLoadBalancerRequest{ServiceKey: "ns/svc", Ports: validPorts()}); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := srv.DeleteLoadBalancer(ctx, &routerv1.DeleteLoadBalancerRequest{ServiceKey: "ns/svc"}); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := srv.GetLoadBalancer(ctx, &routerv1.GetLoadBalancerRequest{ServiceKey: "ns/svc"}); codeOf(err) != codes.NotFound {
			t.Fatalf("expected NotFound after delete, got %v", err)
		}
	})
}

func TestHealth(t *testing.T) {
	ctx := context.Background()

	t.Run("zero without callback", func(t *testing.T) {
		srv, _ := testServer()
		resp, err := srv.Health(ctx, &routerv1.HealthRequest{})
		if err != nil {
			t.Fatalf("health: %v", err)
		}
		if !resp.Healthy || resp.ActiveMacvlans != 0 {
			t.Fatalf("unexpected health response: %+v", resp)
		}
	})

	t.Run("reports callback count", func(t *testing.T) {
		srv, _ := testServer()
		srv.ActiveMacvlanFunc = func() int32 { return 3 }
		resp, err := srv.Health(ctx, &routerv1.HealthRequest{})
		if err != nil {
			t.Fatalf("health: %v", err)
		}
		if resp.ActiveMacvlans != 3 {
			t.Fatalf("expected 3 macvlans, got %d", resp.ActiveMacvlans)
		}
	})
}

// -- geo-blocking tests --

func newGeoTestServer(t *testing.T, geoEnabled bool) (*RouterServer, *state.State) {
	t.Helper()
	srv, st := testServer()
	srv.GeoEnabled = geoEnabled
	return srv, st
}

func TestCreateLoadBalancer_StoresGeos(t *testing.T) {
	srv, st := newGeoTestServer(t, true)
	_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
		ServiceKey:  "ns/svc",
		Ports:       validPorts(),
		AllowedGeos: []string{"us", " ca ", "US"},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, ok := st.GetService("ns/svc")
	if !ok {
		t.Fatal("service not stored")
	}
	if len(svc.AllowedGeos) != 2 || svc.AllowedGeos[0] != "US" || svc.AllowedGeos[1] != "CA" {
		t.Fatalf("stored geos = %v", svc.AllowedGeos)
	}
}

func TestUpdateLoadBalancer_ReplacesGeos(t *testing.T) {
	srv, st := newGeoTestServer(t, true)
	if _, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
		ServiceKey: "ns/svc", Ports: validPorts(), AllowedGeos: []string{"US"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := srv.UpdateLoadBalancer(context.Background(), &routerv1.UpdateLoadBalancerRequest{
		ServiceKey: "ns/svc", Ports: validPorts(), AllowedGeos: []string{"CA"},
	}); err != nil {
		t.Fatal(err)
	}
	svc, _ := st.GetService("ns/svc")
	if len(svc.AllowedGeos) != 1 || svc.AllowedGeos[0] != "CA" {
		t.Fatalf("updated geos = %v", svc.AllowedGeos)
	}

	// Empty list clears the allow-list (allow all).
	if _, err := srv.UpdateLoadBalancer(context.Background(), &routerv1.UpdateLoadBalancerRequest{
		ServiceKey: "ns/svc", Ports: validPorts(),
	}); err != nil {
		t.Fatal(err)
	}
	svc, _ = st.GetService("ns/svc")
	if len(svc.AllowedGeos) != 0 {
		t.Fatalf("geos not cleared: %v", svc.AllowedGeos)
	}
}

func TestGetAndList_ReturnGeos(t *testing.T) {
	srv, _ := newGeoTestServer(t, true)
	if _, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
		ServiceKey: "ns/svc", Ports: validPorts(), AllowedGeos: []string{"US", "CA"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := srv.GetLoadBalancer(context.Background(), &routerv1.GetLoadBalancerRequest{ServiceKey: "ns/svc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.AllowedGeos) != 2 || got.AllowedGeos[0] != "US" || got.AllowedGeos[1] != "CA" {
		t.Fatalf("Get geos = %v", got.AllowedGeos)
	}

	list, err := srv.ListLoadBalancers(context.Background(), &routerv1.ListLoadBalancersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.LoadBalancers) != 1 || len(list.LoadBalancers[0].AllowedGeos) != 2 {
		t.Fatalf("List geos = %v", list.LoadBalancers)
	}
}

func TestGeosRequireMaxMind(t *testing.T) {
	srv, st := newGeoTestServer(t, false)

	_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
		ServiceKey: "ns/svc", Ports: validPorts(), AllowedGeos: []string{"US"},
	})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("Create error = %v, want InvalidArgument", err)
	}
	if st.HasService("ns/svc") {
		t.Fatal("state was mutated on a rejected create")
	}

	// Empty geos still work without MaxMind (backward compatibility).
	if _, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
		ServiceKey: "ns/open", Ports: validPorts(),
	}); err != nil {
		t.Fatalf("create without geos: %v", err)
	}

	_, err = srv.UpdateLoadBalancer(context.Background(), &routerv1.UpdateLoadBalancerRequest{
		ServiceKey: "ns/open", Ports: validPorts(), AllowedGeos: []string{"US"},
	})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("Update error = %v, want InvalidArgument", err)
	}
	svc, _ := st.GetService("ns/open")
	if len(svc.AllowedGeos) != 0 {
		t.Fatalf("state was mutated on a rejected update: %v", svc.AllowedGeos)
	}
}

func TestInvalidGeoCodeRejected(t *testing.T) {
	srv, _ := newGeoTestServer(t, true)
	_, err := srv.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
		ServiceKey: "ns/svc", Ports: validPorts(), AllowedGeos: []string{"USA"},
	})
	if codeOf(err) != codes.InvalidArgument {
		t.Fatalf("error = %v, want InvalidArgument", err)
	}
}
