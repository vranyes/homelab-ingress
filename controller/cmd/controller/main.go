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
	var pollInterval time.Duration
	var metricsAddr string
	var tokenPath string

	flag.StringVar(&routerAddr, "router-addr", config.DefaultRouterAddr, "Router service gRPC address")
	flag.DurationVar(&pollInterval, "poll-interval", config.DefaultPollInterval, "Interval for polling IP changes")
	flag.StringVar(&metricsAddr, "metrics-addr", config.DefaultMetricsAddr, "Metrics bind address")
	flag.StringVar(&tokenPath, "token-path", "", "Path to a Bearer token file for router service authentication")
	flag.Parse()

	cfg := config.Config{
		RouterAddr:   routerAddr,
		PollInterval: pollInterval,
		MetricsAddr:  metricsAddr,
		TokenPath:    tokenPath,
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

	routerClient, err := router.NewClient(cfg.RouterAddr, cfg.TokenPath)
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

	setupLog.Info("starting manager", "router-addr", cfg.RouterAddr, "poll-interval", cfg.PollInterval)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
