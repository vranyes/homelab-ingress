package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/vranyes/homelab-ingress/controller/internal/config"
	"github.com/vranyes/homelab-ingress/controller/internal/controller"
	"github.com/vranyes/homelab-ingress/controller/internal/router"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	var routerAddr string
	var wsEventAddr string
	var pollInterval time.Duration
	var metricsAddr string
	var tokenPath string
	var caPath string

	flag.StringVar(&routerAddr, "router-addr", config.DefaultRouterAddr, "Router service gRPC address")
	flag.StringVar(&wsEventAddr, "ws-event-addr", config.DefaultWSEventAddr, "WebSocket event address (e.g. localhost:50052)")
	flag.DurationVar(&pollInterval, "poll-interval", config.DefaultPollInterval, "Interval for polling IP changes")
	flag.StringVar(&metricsAddr, "metrics-addr", config.DefaultMetricsAddr, "Metrics bind address")
	flag.StringVar(&tokenPath, "token-path", "", "Path to a Bearer token file for router service authentication")
	flag.StringVar(&caPath, "ca-path", "", "Path to CA certificate for verifying router service TLS")
	flag.Parse()

	cfg := config.Config{
		RouterAddr:   routerAddr,
		WSEventAddr:  wsEventAddr,
		PollInterval: pollInterval,
		MetricsAddr:  metricsAddr,
		TokenPath:    tokenPath,
		CAPath:       caPath,
	}

	if err := config.Validate(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "invalid config: %v\n", err)
		os.Exit(1)
	}

	opts := zap.Options{
		Development: true,
		TimeEncoder: zapcore.TimeEncoderOfLayout(time.RFC3339),
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	setupLog := ctrl.Log.WithName("setup")

	routerClient, err := router.NewClient(cfg.RouterAddr, cfg.TokenPath, cfg.CAPath)
	if err != nil {
		setupLog.Error(err, "failed to create router client")
		os.Exit(1)
	}
	defer routerClient.Close()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: server.Options{
			BindAddress: cfg.MetricsAddr,
		},
		LeaderElection:         false,
		LeaderElectionID:       "homelab-ingress-controller",
		HealthProbeBindAddress: ":8081",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.ServiceReconciler{
		Client:       mgr.GetClient(),
		RouterClient: routerClient,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create service controller")
		os.Exit(1)
	}

	poller := &controller.IPPoller{
		Client:       mgr.GetClient(),
		RouterClient: routerClient,
		Interval:     cfg.PollInterval,
	}
	if err := mgr.Add(poller); err != nil {
		setupLog.Error(err, "unable to add IP poller")
		os.Exit(1)
	}

	if cfg.WSEventAddr != "" {
		wsClient := router.NewWSEventClient(mgr.GetClient(), cfg.WSEventAddr, cfg.TokenPath, cfg.CAPath)
		if err := mgr.Add(wsClient); err != nil {
			setupLog.Error(err, "unable to add WebSocket event client")
			os.Exit(1)
		}
		setupLog.Info("WebSocket event client enabled", "addr", cfg.WSEventAddr)
	}

	setupLog.Info("starting manager", "router-addr", cfg.RouterAddr, "poll-interval", cfg.PollInterval)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
