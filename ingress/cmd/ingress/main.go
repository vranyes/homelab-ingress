//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/vranyes/homelab-ingress/ingress/internal/auth"
	"github.com/vranyes/homelab-ingress/ingress/internal/certgen"
	"github.com/vranyes/homelab-ingress/ingress/internal/config"
	"github.com/vranyes/homelab-ingress/ingress/internal/dhcp"
	"github.com/vranyes/homelab-ingress/ingress/internal/macvlan"
	"github.com/vranyes/homelab-ingress/ingress/internal/router"
	"github.com/vranyes/homelab-ingress/ingress/internal/router/nftables"
	"github.com/vranyes/homelab-ingress/ingress/internal/service"
	"github.com/vranyes/homelab-ingress/ingress/internal/state"
	routerv1 "github.com/vranyes/homelab-ingress/ingress/pkg/api/router/v1"
)

const configPathDefault = "/etc/homelab-ingress/config.yaml"

func main() {
	configPath := flag.String("config", configPathDefault, "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := loadConfig(logger, *configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	st, err := loadState(logger, cfg.BaseInterface)
	if err != nil {
		logger.Error("failed to load state", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	linkMgr := macvlan.NewNetlinkManager(st, logger)
	go linkMgr.Run(st.Subscribe())

	dhcpMgr := dhcp.NewManager(st, logger)
	go dhcpMgr.Run(st.Subscribe())
	defer dhcpMgr.StopAll()
	defer dhcpMgr.Stop()

	var nftOpts []nftables.Option
	for _, r := range cfg.Masquerade {
		nftOpts = append(nftOpts, nftables.WithMasquerade(r.InputDevice, r.OutputDevice))
	}
	iml := nftables.NewManager(logger, nftOpts...)
	rmgr := router.NewManager(iml, st, logger)
	go rmgr.Run(st.Subscribe())

	// reuse the same store instance created earlier
	srv := service.NewRouterServer(st, logger)
	srv.ActiveMacvlanFunc = linkMgr.ActiveCount

	// Load or create a root CA and generate a leaf TLS certificate for the server.
	certDir := filepath.Dir(*configPath)
	serverNames := []string{stripPort(cfg.GRPC.Listen)}
	tlsCert, caPEM, err := certgen.LoadOrCreate(certDir, serverNames)
	if err != nil {
		logger.Error("failed to load or create TLS certificate", "error", err)
		os.Exit(1)
	}

	// Write the CA PEM so clients can pick it up.
	caOut := filepath.Join(certDir, "ca.pem")
	if _, err := os.Stat(caOut); os.IsNotExist(err) {
		if err := os.WriteFile(caOut, caPEM, 0644); err != nil {
			logger.Error("failed to write CA certificate", "error", err)
			os.Exit(1)
		}
		logger.Info("CA certificate written", "path", caOut)
	}

	// Always use TLS for the gRPC server.
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{*tlsCert},
		MinVersion:   tls.VersionTLS12,
	}
	opts := []grpc.ServerOption{grpc.Creds(credentials.NewTLS(tlsCfg))}

	// Load custom root CA for the OIDC issuer if configured.
	var trustedRoot *x509.CertPool
	if cfg.Auth.TrustedRoot != "" {
		trustedRoot, err = certgen.LoadCACert(cfg.Auth.TrustedRoot)
		if err != nil {
			logger.Error("failed to load trusted root CA for OIDC issuer", "error", err)
			os.Exit(1)
		}
	}

	unary, stream, err := auth.NewInterceptors(context.Background(), cfg.Auth.Issuer, cfg.Auth.Audience, cfg.Auth.AllowedSubjects, trustedRoot, logger)
	if err != nil {
		logger.Error("failed to initialize auth", "error", err)
		os.Exit(1)
	}
	opts = append(opts, grpc.UnaryInterceptor(unary), grpc.StreamInterceptor(stream))

	grpcServer := grpc.NewServer(opts...)
	routerv1.RegisterRouterServiceServer(grpcServer, srv)

	listener, err := net.Listen("tcp", cfg.GRPC.Listen)
	if err != nil {
		logger.Error("failed to listen", "error", err)
		os.Exit(1)
	}
	logger.Info("gRPC server started with TLS", "listen", cfg.GRPC.Listen)
	go func() {
		if err := grpcServer.Serve(listener); err != nil {
			logger.Error("gRPC server error", "error", err)
			os.Exit(1)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	logger.Info("received signal, shutting down", "signal", sig)

	grpcServer.GracefulStop()
	logger.Info("gRPC server stopped, cleaning up")

	iml.Cleanup()
	linkMgr.DeleteAll()
	linkMgr.ParentSetDown()

	logger.Info("shutdown complete")
}

func loadConfig(logger *slog.Logger, path string) (config.Config, error) {
	cfg, err := config.LoadConfig(path)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Info("config file not found, using defaults", "path", path)
			cfg = config.DefaultConfig()
			if cfg.BaseInterface == "" {
				return config.Config{}, fmt.Errorf("base_interface is required (set via config or env)")
			}
			return cfg, nil
		}
		return config.Config{}, err
	}
	logger.Info("config loaded", "base_interface", cfg.BaseInterface, "grpc_listen", cfg.GRPC.Listen)
	return cfg, nil
}

func loadState(logger *slog.Logger, baseIface string) (*state.State, error) {
	if baseIface == "" {
		return nil, fmt.Errorf("base_interface is required")
	}

	store, err := state.NewStore("/etc/homelab-ingress/state.db", logger)
	if err != nil {
		return nil, fmt.Errorf("create store: %w", err)
	}

	st := state.NewState(baseIface, store, logger)

	services, err := store.LoadServices()
	if err != nil {
		logger.Error("failed to load persisted services", "error", err)
	} else {
		for name, svc := range services {
			st.Services[name] = svc
			logger.Info("restored service from persistence", "service", name)
		}
	}

	logger.Info("state initialized", "restored_services", len(st.Services))
	return st, nil
}

func stripPort(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return strings.TrimSpace(host)
}
