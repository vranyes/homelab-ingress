//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	"github.com/vranyes/homelab-ingress/ingress/internal/auth"
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

	iml := nftables.NewManager(logger)
	rmgr := router.NewManager(iml, st, logger)
	go rmgr.Run(st.Subscribe())

	// reuse the same store instance created earlier
	srv := service.NewRouterServer(st, logger)
	srv.ActiveMacvlanFunc = linkMgr.ActiveCount

	listener, err := net.Listen("tcp", cfg.GRPC.Listen)
	if err != nil {
		logger.Error("failed to listen", "error", err)
		os.Exit(1)
	}

	// Configure gRPC server options, including auth interceptors when configured
	var opts []grpc.ServerOption
	if cfg.Auth.Issuer != "" {
		unary, stream, err := auth.NewInterceptors(context.Background(), cfg.Auth.Issuer, cfg.Auth.Audience, cfg.Auth.AllowedSubjects, logger)
		if err != nil {
			logger.Error("failed to initialize auth", "error", err)
			os.Exit(1)
		}
		if unary != nil {
			opts = append(opts, grpc.UnaryInterceptor(unary), grpc.StreamInterceptor(stream))
		}
	}

	grpcServer := grpc.NewServer(opts...)
	routerv1.RegisterRouterServiceServer(grpcServer, srv)

	go func() {
		logger.Info("gRPC server started", "listen", cfg.GRPC.Listen)
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
