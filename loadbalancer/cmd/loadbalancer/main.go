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

	"github.com/vranyes/homelab-ingress/loadbalancer/internal/auth"
	"github.com/vranyes/homelab-ingress/loadbalancer/internal/config"
	"github.com/vranyes/homelab-ingress/loadbalancer/internal/macvlan"
	"github.com/vranyes/homelab-ingress/loadbalancer/internal/service"
	"github.com/vranyes/homelab-ingress/loadbalancer/internal/state"
	routerv1 "github.com/vranyes/homelab-ingress/loadbalancer/pkg/api/router/v1"
)

const configPathDefault = "/etc/homelab-lb/config.yaml"

func main() {
	configPath := flag.String("config", configPathDefault, "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := loadConfig(logger, *configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	store := state.NewStore(cfg.StateFile)
	st, err := loadState(logger, store, cfg.BaseInterface)
	if err != nil {
		logger.Error("failed to load state", "error", err)
		os.Exit(1)
	}
	st.UpdateCh = make(chan state.LBNotification)

	linkMgr := macvlan.NewNetlinkManagerWithLogger(st, logger)
	go linkMgr.Run(st.UpdateCh)

	// Create DHCP manager with factory that updates firewall rules on IP change
	// dhcpMgr := dhcp.NewManagerWithFactoryAndLogger(
	// 	func(iface string, ip net.IP) {
	// 		key := ifaceToServiceKey(st, iface)
	// 		if key == "" {
	// 			logger.Warn("could not find service key for interface", "interface", iface)
	// 			return
	// 		}

	// 		// Update state with new IP
	// 		state.UpdateIP(st, key, ip)
	// 	},
	// 	func(iface string, mac net.HardwareAddr, onChange dhcp.IPChangeFunc) dhcp.Client {
	// 		return dhcp.NewRealClientWithLogger(iface, mac, onChange, logger)
	// 	},
	// 	logger,
	// )
	// dhcpMgr.Watch()
	// defer dhcpMgr.StopAll()

	// reuse the same store instance created earlier
	srv := service.NewRouterServerWithLogger(store, st, logger)

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

func loadState(logger *slog.Logger, store *state.Store, baseIface string) (*state.State, error) {
	st, err := store.Load()
	if err != nil {
		if os.IsNotExist(err) {
			logger.Info("no existing state, creating new")
			return state.NewState(baseIface), nil
		}
		return nil, fmt.Errorf("load state: %w", err)
	}
	logger.Info("state loaded", "services", len(st.Services))
	return st, nil
}

func ifaceToServiceKey(st *state.State, iface string) string {
	// Snapshot state to iterate safely.
	snap := st.Clone()
	for key, svc := range snap.Services {
		if svc.Interface == iface {
			return key
		}
	}
	return ""
}
