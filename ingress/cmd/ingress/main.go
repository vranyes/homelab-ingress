//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/vranyes/homelab-ingress/ingress/internal/auth"
	"github.com/vranyes/homelab-ingress/ingress/internal/certgen"
	"github.com/vranyes/homelab-ingress/ingress/internal/config"
	"github.com/vranyes/homelab-ingress/ingress/internal/dhcp"
	"github.com/vranyes/homelab-ingress/ingress/internal/geo"
	"github.com/vranyes/homelab-ingress/ingress/internal/geoip"
	"github.com/vranyes/homelab-ingress/ingress/internal/health"
	"github.com/vranyes/homelab-ingress/ingress/internal/macvlan"
	"github.com/vranyes/homelab-ingress/ingress/internal/metrics"
	"github.com/vranyes/homelab-ingress/ingress/internal/router"
	"github.com/vranyes/homelab-ingress/ingress/internal/router/nftables"
	"github.com/vranyes/homelab-ingress/ingress/internal/service"
	"github.com/vranyes/homelab-ingress/ingress/internal/state"
	"github.com/vranyes/homelab-ingress/ingress/internal/ws"
	routerv1 "github.com/vranyes/homelab-ingress/ingress/pkg/api/router/v1"
)

const configPathDefault = "/etc/homelab-ingress/config.yaml"

func main() {
	configPath := flag.String("config", configPathDefault, "path to config file")
	stateDir := flag.String("state-dir", "/etc/homelab-ingress", "path to state directory")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := loadConfig(logger, *configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	cfg.GeoIP.ApplyDefaults(*stateDir)

	// Load or create the root CA before touching state. On a first boot the
	// state directory is empty, so a CA is generated; on every later boot the
	// existing CA is reused. The fingerprint is logged so operators can prove
	// whether a restart rotated it.
	serverNames := []string{stripPort(cfg.GRPC.Listen)}
	tlsCert, caPEM, err := certgen.LoadOrCreate(*stateDir, serverNames)
	if err != nil {
		logger.Error("failed to load or create TLS certificate", "error", err)
		os.Exit(1)
	}
	caOut := filepath.Join(*stateDir, "ca.pem")
	if _, statErr := os.Stat(caOut); os.IsNotExist(statErr) {
		if err := os.WriteFile(caOut, caPEM, 0644); err != nil {
			logger.Error("failed to write CA certificate", "error", err)
			os.Exit(1)
		}
		logger.Info("CA certificate written", "path", caOut)
	}
	caSum := sha256.Sum256(caPEM)
	logger.Info("CA certificate ready",
		"path", caOut,
		"state_dir", *stateDir,
		"sha256", hex.EncodeToString(caSum[:]))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, err := loadState(logger, *stateDir, cfg.BaseInterface)
	if err != nil {
		logger.Error("failed to load state", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	// Prometheus exposition. An empty listen address disables the
	// endpoint; startup must not depend on it.
	metrics.Start(cfg.Metrics.Listen, logger)

	linkMgr := macvlan.NewNetlinkManager(st, logger)
	go linkMgr.Run(st.Subscribe())

	dhcpMgr := dhcp.NewManager(st, logger)
	go dhcpMgr.Run(st.Subscribe())
	defer dhcpMgr.StopAll()
	defer dhcpMgr.Stop()

	// Geo-blocking is optional (ADR 0003, ADR 0006). When a country database
	// is configured, geo.Manager owns the country→CIDR cache and pushes
	// service set refreshes into the nftables manager (ADR 0004).
	var geoMgr *geo.Manager
	var mm *geoip.Manager
	if cfg.GeoIP.Enabled() {
		mm, err = geoip.New(cfg.GeoIP, logger)
		if err != nil {
			logger.Error("failed to initialize geoip", "error", err)
			os.Exit(1)
		}
		logger.Info("geo-blocking enabled",
			"provider", cfg.GeoIP.Provider,
			"cache_path", cfg.GeoIP.CachePath,
			"mock", cfg.GeoIP.Mock())
		if cfg.GeoIP.Provider == config.ProviderDBIP {
			// DB-IP Lite is CC BY 4.0; attribution is required.
			logger.Info("geo data by DB-IP", "attribution", "https://db-ip.com")
		}
		geoMgr = geo.NewManager(mm, st, logger, cfg.GeoIP.CachePath)
	}

	// Active TCP health-checking (ADR 0009). The checker probes backends on
	// its own ticker; rule planning consults it via WithHealthChecker, and
	// health flips trigger a reconcile so the DNAT maps converge between
	// state events. It shares the main ctx, so shutdown cancels it cleanly.
	checker := health.NewChecker(health.Config{
		Interval:         cfg.Healthcheck.Interval,
		Timeout:          cfg.Healthcheck.Timeout,
		FailThreshold:    cfg.Healthcheck.FailThreshold,
		SuccessThreshold: cfg.Healthcheck.SuccessThreshold,
	}, logger)

	var nftOpts []nftables.Option
	nftOpts = append(nftOpts, nftables.WithHealthChecker(checker))
	for _, r := range cfg.Masquerade {
		nftOpts = append(nftOpts, nftables.WithMasquerade(r.InputDevice, r.OutputDevice))
	}
	if geoMgr != nil {
		nftOpts = append(nftOpts, nftables.WithGeoLookup(geoMgr.CountryCIDRs))
	}
	iml := nftables.NewManager(logger, nftOpts...)
	if geoMgr != nil {
		geoMgr.OnRebuild(func(svcCIDRs map[string][]*net.IPNet) {
			if err := iml.RefreshGeoSets(svcCIDRs); err != nil {
				logger.Error("failed to refresh geo sets", "error", err)
			}
		})
		go geoMgr.Run(ctx)
		go mm.Run(ctx, cfg.GeoIP.RefreshInterval)
	}
	rmgr := router.NewManager(iml, st, logger)
	go rmgr.Run(st.Subscribe())

	checker.OnChange = func() {
		// Reconcile off the probe loop: a health flip rebuilds the DNAT
		// maps from the full backend list minus unhealthy entries.
		go func() {
			if err := rmgr.Reconcile(); err != nil {
				logger.Error("health-triggered reconcile failed", "error", err)
			}
		}()
	}
	go checker.Run(ctx, st)

	// reuse the same store instance created earlier
	srv := service.NewRouterServer(st, logger)
	srv.ActiveMacvlanFunc = linkMgr.ActiveCount
	srv.GeoEnabled = cfg.GeoIP.Enabled()

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

	verifier, err := auth.NewTokenVerifier(context.Background(), cfg.Auth.Issuer, cfg.Auth.Audience, cfg.Auth.AllowedSubjects, trustedRoot, logger)
	if err != nil {
		logger.Error("failed to create token verifier", "error", err)
		os.Exit(1)
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

	if cfg.WS.Listen != "" {
		wsSrv := ws.NewServer(cfg.WS.Listen, verifier, st, tlsCfg, logger)
		go func() {
			if err := wsSrv.Start(); err != nil {
				logger.Error("WebSocket server error", "error", err)
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	logger.Info("received signal, shutting down", "signal", sig)
	cancel()

	// Ordered shutdown. The naive order (linkMgr.DeleteAll before the
	// deferred dhcpMgr.StopAll) tears interfaces down while the per-service
	// DHCP clients are still trying to send their DHCP RELEASE frames,
	// which the kernel then rejects with "no such network interface" via
	// `nclient4.New(iface, ...)`. Stop the DHCP clients first so
	// `real.go::release()` runs while every lb-* interface is still UP,
	// then drain in-flight frames, then remove the interfaces, then drop
	// the nftables table.
	grpcServer.GracefulStop()
	logger.Info("gRPC server stopped, dhcp clients stopping next")

	dhcpMgr.Stop()
	dhcpMgr.StopAll()

	dhcpReleaseDrain := 2 * time.Second
	logger.Info("waiting for DHCP RELEASE frames to egress", "drain", dhcpReleaseDrain)
	time.Sleep(dhcpReleaseDrain)

	linkMgr.DeleteAll()
	linkMgr.ParentSetDown()
	iml.Cleanup()

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

func loadState(logger *slog.Logger, stateDir, baseIface string) (*state.State, error) {
	if baseIface == "" {
		return nil, fmt.Errorf("base_interface is required")
	}

	store, err := state.NewStore(filepath.Join(stateDir, "state.db"), logger)
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
