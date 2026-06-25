package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	routerv1 "github.com/vranyes/homelab-ingress/ingress/pkg/api/router/v1"
)

const defaultAddr = "localhost:50051"
const defaultCACert = "/etc/homelab-ingress/ca.pem"

type tokenCredential struct {
	token string
}

func (t *tokenCredential) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + t.token,
	}, nil
}

func (t *tokenCredential) RequireTransportSecurity() bool {
	return true
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "health":
		cmdHealth(os.Args[2:])
	case "create":
		cmdCreate(os.Args[2:])
	case "update":
		cmdUpdate(os.Args[2:])
	case "delete":
		cmdDelete(os.Args[2:])
	case "get":
		cmdGet(os.Args[2:])
	case "list":
		cmdList(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

type commonFlags struct {
	addr    string
	token   string
	caCert  string
}

func addCommonFlags(fs *flag.FlagSet) *commonFlags {
	return &commonFlags{
		addr:   *fs.String("addr", defaultAddr, "gRPC address"),
		token:  *fs.String("token", os.Getenv("ROUTER_AUTH_TOKEN"), "OIDC Bearer token"),
		caCert: *fs.String("ca-cert", defaultCACert, "Path to server CA certificate"),
	}
}

func printUsage() {
	fmt.Println("Usage: router-cli <command> [flags]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  health                    Check router health")
	fmt.Println("  create -service <key> -port <proto:ext:pod:[podIP,...]>")
	fmt.Println("                            Create a load balancer")
	fmt.Println("  update -service <key> -port <proto:ext:pod:[podIP,...]>")
	fmt.Println("                            Update a load balancer")
	fmt.Println("  delete -service <key>     Delete a load balancer")
	fmt.Println("  get -service <key>        Get a load balancer")
	fmt.Println("  list                      List all load balancers")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  -addr string    gRPC address (default \"localhost:50051\")")
	fmt.Println("  -token string   OIDC Bearer token for authentication")
	fmt.Println("  -ca-cert string Path to server CA certificate (default \"/etc/homelab-ingress/ca.pem\")")
	fmt.Println("  -service string Service key (e.g. default/myapp)")
	fmt.Println("  -port string    Port mapping: protocol,external_port,target_ip:target_port,...")
	fmt.Println("                  Can be specified multiple times")
	fmt.Println("                  Example: tcp,80,10.42.0.10:8000")
	fmt.Println("                  Example: tcp,443,10.42.0.10:8443,10.42.0.11:9443")
}

func dial(addr, token, caCertPath string) (*grpc.ClientConn, routerv1.RouterServiceClient, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

	pool := x509.NewCertPool()
	caData, err := os.ReadFile(caCertPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read CA cert %q: %v\n", caCertPath, err)
		os.Exit(1)
	}
	if !pool.AppendCertsFromPEM(caData) {
		fmt.Fprintf(os.Stderr, "no valid CA cert in %q\n", caCertPath)
		os.Exit(1)
	}
	tlsCfg := &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(&tokenCredential{token: token}))
	}
	conn, err := grpc.DialContext(ctx, addr, opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect: %v\n", err)
		os.Exit(1)
	}
	return conn, routerv1.NewRouterServiceClient(conn), cancel
}

func cmdHealth(args []string) {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	cf := addCommonFlags(fs)
	_ = fs.Parse(args)

	conn, client, cancel := dial(cf.addr, cf.token, cf.caCert)
	defer conn.Close()
	defer cancel()

	resp, err := client.Health(context.Background(), &routerv1.HealthRequest{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "health check failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Healthy:        %v\n", resp.Healthy)
	fmt.Printf("Base Interface: %s\n", resp.BaseInterface)
	fmt.Printf("Active Macvlans: %d\n", resp.ActiveMacvlans)
}

func cmdCreate(args []string) {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	cf := addCommonFlags(fs)
	service := fs.String("service", "", "service key (required)")
	var ports stringSliceFlag
	fs.Var(&ports, "port", "port mapping (protocol:extPort:podPort:[podIPs])")
	_ = fs.Parse(args)

	if *service == "" {
		fmt.Fprintln(os.Stderr, "-service is required")
		os.Exit(1)
	}
	if len(ports) == 0 {
		fmt.Fprintln(os.Stderr, "at least one -port is required")
		os.Exit(1)
	}

	parsedPorts := parsePortMappings(ports)

	conn, client, cancel := dial(cf.addr, cf.token, cf.caCert)
	defer conn.Close()
	defer cancel()

	resp, err := client.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
		ServiceKey: *service,
		Ports:      parsedPorts,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Created: %s\n", *service)
	fmt.Printf("Assigned IP: %s\n", resp.AssignedIp)
}

func cmdUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	cf := addCommonFlags(fs)
	service := fs.String("service", "", "service key (required)")
	var ports stringSliceFlag
	fs.Var(&ports, "port", "port mapping (protocol:extPort:podPort:[podIPs])")
	_ = fs.Parse(args)

	if *service == "" {
		fmt.Fprintln(os.Stderr, "-service is required")
		os.Exit(1)
	}
	if len(ports) == 0 {
		fmt.Fprintln(os.Stderr, "at least one -port is required")
		os.Exit(1)
	}

	parsedPorts := parsePortMappings(ports)

	conn, client, cancel := dial(cf.addr, cf.token, cf.caCert)
	defer conn.Close()
	defer cancel()

	_, err := client.UpdateLoadBalancer(context.Background(), &routerv1.UpdateLoadBalancerRequest{
		ServiceKey: *service,
		Ports:      parsedPorts,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "update failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Updated: %s\n", *service)
}

func cmdDelete(args []string) {
	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	cf := addCommonFlags(fs)
	service := fs.String("service", "", "service key (required)")
	_ = fs.Parse(args)

	if *service == "" {
		fmt.Fprintln(os.Stderr, "-service is required")
		os.Exit(1)
	}

	conn, client, cancel := dial(cf.addr, cf.token, cf.caCert)
	defer conn.Close()
	defer cancel()

	_, err := client.DeleteLoadBalancer(context.Background(), &routerv1.DeleteLoadBalancerRequest{
		ServiceKey: *service,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Deleted: %s\n", *service)
}

func cmdGet(args []string) {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	cf := addCommonFlags(fs)
	service := fs.String("service", "", "service key (required)")
	_ = fs.Parse(args)

	if *service == "" {
		fmt.Fprintln(os.Stderr, "-service is required")
		os.Exit(1)
	}

	conn, client, cancel := dial(cf.addr, cf.token, cf.caCert)
	defer conn.Close()
	defer cancel()

	resp, err := client.GetLoadBalancer(context.Background(), &routerv1.GetLoadBalancerRequest{
		ServiceKey: *service,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "get failed: %v\n", err)
		os.Exit(1)
	}

	printService(resp)
}

func cmdList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	cf := addCommonFlags(fs)
	jsonOutput := fs.Bool("json", false, "output as JSON")
	_ = fs.Parse(args)

	conn, client, cancel := dial(cf.addr, cf.token, cf.caCert)
	defer conn.Close()
	defer cancel()

	resp, err := client.ListLoadBalancers(context.Background(), &routerv1.ListLoadBalancersRequest{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "list failed: %v\n", err)
		os.Exit(1)
	}

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(resp)
		return
	}

	if len(resp.LoadBalancers) == 0 {
		fmt.Println("No load balancers found.")
		return
	}

	fmt.Printf("%-30s %-18s %-10s %-15s %s\n", "SERVICE", "IP", "DHCP", "INTERFACE", "PORTS")
	fmt.Println(strings.Repeat("-", 90))
	for _, lb := range resp.LoadBalancers {
		dhcp := "no"
		if lb.DhcpAcquired {
			dhcp = "yes"
		}
		ports := formatPorts(lb.Ports)
		ip := lb.AssignedIp
		if ip == "" {
			ip = "(pending)"
		}
		fmt.Printf("%-30s %-18s %-10s %-15s %s\n", lb.ServiceKey, ip, dhcp, lb.InterfaceName, ports)
	}
	fmt.Printf("\nTotal: %d\n", len(resp.LoadBalancers))
}

func printService(lb *routerv1.GetLoadBalancerResponse) {
	fmt.Printf("Service Key:    %s\n", lb.ServiceKey)
	fmt.Printf("Assigned IP:    %s\n", valueOrPending(lb.AssignedIp))
	fmt.Printf("Interface:      %s\n", lb.InterfaceName)
	fmt.Printf("DHCP Acquired:  %v\n", lb.DhcpAcquired)
	fmt.Println("Port Mappings:")
	for _, p := range lb.Ports {
		for _, q := range p.Dst {
			fmt.Printf("  %-5s :%d -> %s:%d\n", p.Protocol, p.SrcPort, q.Addr, q.Port)
		}
	}
}

func valueOrPending(s string) string {
	if s == "" {
		return "(pending)"
	}
	return s
}

func formatPorts(ports []*routerv1.PortMapping) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		for _, q := range p.Dst {
			parts[i] = fmt.Sprintf("%s:%d->%s:%d", p.Protocol, p.SrcPort, q.Addr, q.Port)
		}
	}
	return strings.Join(parts, "; ")
}

func parsePortMappings(flags []string) []*routerv1.PortMapping {
	var ports []*routerv1.PortMapping
	for _, f := range flags {
		p := parsePortMapping(f)
		if p != nil {
			ports = append(ports, p)
		}
	}
	return ports
}

func parsePortMapping(s string) *routerv1.PortMapping {
	parts := strings.Split(s, ",")
	if len(parts) < 3 {
		fmt.Fprintf(os.Stderr, "invalid port mapping %q: expected protocol,extPort,targetIP:targetPort,...\n", s)
		os.Exit(1)
	}

	extPort, err := strconv.Atoi(parts[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid external port %q: %v\n", parts[1], err)
		os.Exit(1)
	}

	dsts := []*routerv1.Destination{}
	for _, part := range parts[2:] {
		s := strings.Split(part, ":")
		if len(s) != 2 {
			fmt.Fprintf(os.Stderr, "invalid port mapping %q: expected targetIP:targetPort\n", part)
			os.Exit(1)
		}
		p, err := strconv.Atoi(s[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid port %q: %v\n", s[1], err)
			os.Exit(1)
		}
		dsts = append(dsts, &routerv1.Destination{
			Addr: s[0],
			Port: uint32(p),
		})
	}

	return &routerv1.PortMapping{
		Protocol: parts[0],
		SrcPort:  uint32(extPort),
		Dst:      dsts,
	}
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ", ")
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}
