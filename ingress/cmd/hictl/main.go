package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	routerv1 "github.com/vranyes/homelab-ingress/ingress/pkg/api/router/v1"
)

const (
	defaultAddr    = "localhost:50051"
	defaultCACert  = "/etc/homelab-ingress/ca.pem"
	configFileName = "config"
)

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

type cliFlags struct {
	addr   string
	token  string
	caCert string
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

func defaultConfigDir() string {
	if d, ok := os.LookupEnv("XDG_CONFIG_HOME"); ok {
		return filepath.Join(d, "hictl")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "hictl")
}

func main() {
	var cf cliFlags
	var cfgFile string

	rootCmd := &cobra.Command{
		Use:   "hictl",
		Short: "CLI for managing the homelab ingress router",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return initConfig(cfgFile)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
		SilenceUsage: true,
	}

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: ~/.config/hictl/config.yaml)")
	rootCmd.PersistentFlags().StringVar(&cf.addr, "addr", defaultAddr, "gRPC address (env ROUTER_ADDR)")
	rootCmd.PersistentFlags().StringVar(&cf.token, "token", "", "OIDC Bearer token (env ROUTER_AUTH_TOKEN)")
	rootCmd.PersistentFlags().StringVar(&cf.caCert, "ca-cert", defaultCACert, "Path to server CA certificate (env ROUTER_CA_CERT)")

	_ = viper.BindPFlag("addr", rootCmd.PersistentFlags().Lookup("addr"))
	_ = viper.BindPFlag("token", rootCmd.PersistentFlags().Lookup("token"))
	_ = viper.BindPFlag("ca_cert", rootCmd.PersistentFlags().Lookup("ca-cert"))

	_ = viper.BindEnv("addr", "ROUTER_ADDR")
	_ = viper.BindEnv("token", "ROUTER_AUTH_TOKEN")
	_ = viper.BindEnv("ca_cert", "ROUTER_CA_CERT")

	rootCmd.AddCommand(newHealthCmd(&cf))
	rootCmd.AddCommand(newCreateCmd(&cf))
	rootCmd.AddCommand(newUpdateCmd(&cf))
	rootCmd.AddCommand(newDeleteCmd(&cf))
	rootCmd.AddCommand(newGetCmd(&cf))
	rootCmd.AddCommand(newListCmd(&cf))
	rootCmd.AddCommand(newCompletionCmd(rootCmd))

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func initConfig(cfgFile string) error {
	viper.SetDefault("addr", defaultAddr)
	viper.SetDefault("ca_cert", defaultCACert)

	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		configDir := defaultConfigDir()
		viper.AddConfigPath(configDir)
		viper.SetConfigName(configFileName)
	}

	viper.SetEnvPrefix("HICTL")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	_ = viper.ReadInConfig()

	return nil
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ", ")
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func (s *stringSliceFlag) Type() string {
	return "stringSlice"
}

func resolveFlags(cf *cliFlags) {
	if v := viper.GetString("addr"); v != "" {
		cf.addr = v
	}
	if v := viper.GetString("token"); v != "" {
		cf.token = v
	}
	if v := viper.GetString("ca_cert"); v != "" {
		cf.caCert = v
	}
}

func newHealthCmd(cf *cliFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Check router health",
		Run: func(cmd *cobra.Command, args []string) {
			resolveFlags(cf)
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
		},
	}
}

func newCreateCmd(cf *cliFlags) *cobra.Command {
	var service string
	var ports stringSliceFlag

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a load balancer",
		Run: func(cmd *cobra.Command, args []string) {
			resolveFlags(cf)
			if service == "" {
				fmt.Fprintln(os.Stderr, "Error: --service is required")
				os.Exit(1)
			}
			if len(ports) == 0 {
				fmt.Fprintln(os.Stderr, "Error: at least one --port is required")
				os.Exit(1)
			}

			parsedPorts := parsePortMappings(ports)

			conn, client, cancel := dial(cf.addr, cf.token, cf.caCert)
			defer conn.Close()
			defer cancel()

			resp, err := client.CreateLoadBalancer(context.Background(), &routerv1.CreateLoadBalancerRequest{
				ServiceKey: service,
				Ports:      parsedPorts,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "create failed: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Created: %s\n", service)
			fmt.Printf("Assigned IP: %s\n", resp.AssignedIp)
		},
	}

	cmd.Flags().StringVar(&service, "service", "", "Service key (e.g. default/myapp)")
	cmd.Flags().Var(&ports, "port", "Port mapping: protocol,external_port,target_ip:target_port,... (can be specified multiple times)")

	return cmd
}

func newUpdateCmd(cf *cliFlags) *cobra.Command {
	var service string
	var ports stringSliceFlag

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update a load balancer",
		Run: func(cmd *cobra.Command, args []string) {
			resolveFlags(cf)
			if service == "" {
				fmt.Fprintln(os.Stderr, "Error: --service is required")
				os.Exit(1)
			}
			if len(ports) == 0 {
				fmt.Fprintln(os.Stderr, "Error: at least one --port is required")
				os.Exit(1)
			}

			parsedPorts := parsePortMappings(ports)

			conn, client, cancel := dial(cf.addr, cf.token, cf.caCert)
			defer conn.Close()
			defer cancel()

			_, err := client.UpdateLoadBalancer(context.Background(), &routerv1.UpdateLoadBalancerRequest{
				ServiceKey: service,
				Ports:      parsedPorts,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "update failed: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Updated: %s\n", service)
		},
	}

	cmd.Flags().StringVar(&service, "service", "", "Service key (e.g. default/myapp)")
	cmd.Flags().Var(&ports, "port", "Port mapping: protocol,external_port,target_ip:target_port,... (can be specified multiple times)")

	return cmd
}

func newDeleteCmd(cf *cliFlags) *cobra.Command {
	var service string

	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a load balancer",
		Run: func(cmd *cobra.Command, args []string) {
			resolveFlags(cf)
			if service == "" {
				fmt.Fprintln(os.Stderr, "Error: --service is required")
				os.Exit(1)
			}

			conn, client, cancel := dial(cf.addr, cf.token, cf.caCert)
			defer conn.Close()
			defer cancel()

			_, err := client.DeleteLoadBalancer(context.Background(), &routerv1.DeleteLoadBalancerRequest{
				ServiceKey: service,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "delete failed: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Deleted: %s\n", service)
		},
	}

	cmd.Flags().StringVar(&service, "service", "", "Service key (e.g. default/myapp)")

	return cmd
}

func newGetCmd(cf *cliFlags) *cobra.Command {
	var service string

	cmd := &cobra.Command{
		Use:   "get",
		Short: "Get a load balancer",
		Run: func(cmd *cobra.Command, args []string) {
			resolveFlags(cf)
			if service == "" {
				fmt.Fprintln(os.Stderr, "Error: --service is required")
				os.Exit(1)
			}

			conn, client, cancel := dial(cf.addr, cf.token, cf.caCert)
			defer conn.Close()
			defer cancel()

			resp, err := client.GetLoadBalancer(context.Background(), &routerv1.GetLoadBalancerRequest{
				ServiceKey: service,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "get failed: %v\n", err)
				os.Exit(1)
			}

			printService(resp)
		},
	}

	cmd.Flags().StringVar(&service, "service", "", "Service key (e.g. default/myapp)")

	return cmd
}

func newListCmd(cf *cliFlags) *cobra.Command {
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all load balancers",
		Run: func(cmd *cobra.Command, args []string) {
			resolveFlags(cf)
			conn, client, cancel := dial(cf.addr, cf.token, cf.caCert)
			defer conn.Close()
			defer cancel()

			resp, err := client.ListLoadBalancers(context.Background(), &routerv1.ListLoadBalancersRequest{})
			if err != nil {
				fmt.Fprintf(os.Stderr, "list failed: %v\n", err)
				os.Exit(1)
			}

			if jsonOutput {
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
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output as JSON")

	return cmd
}

func newCompletionCmd(root *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion scripts",
		Long: `Generate shell completion scripts for hictl.

To load completions:

Bash:

  $ source <(hictl completion bash)

  # To load completions for each session, execute once:
  # Linux:
  $ hictl completion bash > /etc/bash_completion.d/hictl
  # macOS:
  $ hictl completion bash > $(brew --prefix)/etc/bash_completion.d/hictl

Zsh:

  # If shell completion is not already enabled in your environment,
  # you will need to enable it.  You can execute the following once:

  $ echo "autoload -U compinit; compinit" >> ~/.zshrc

  # To load completions for each session, execute once:
  $ hictl completion zsh > "${fpath[1]}/_hictl"

  # You will need to start a new shell for this setup to take effect.

Fish:

  $ hictl completion fish | source

  # To load completions for each session, execute once:
  $ hictl completion fish > ~/.config/fish/completions/hictl.fish

PowerShell:

  PS> hictl completion powershell | Out-String | Invoke-Expression

  # To load completions for every new session, run:
  PS> hictl completion powershell > hictl.ps1
  # and source this file from your PowerShell profile.
`,
		DisableFlagsInUseLine: true,
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(os.Stdout, true)
			case "zsh":
				return root.GenZshCompletion(os.Stdout)
			case "fish":
				return root.GenFishCompletion(os.Stdout, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(os.Stdout)
			default:
				return fmt.Errorf("unsupported shell: %q", args[0])
			}
		},
	}

	return cmd
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
