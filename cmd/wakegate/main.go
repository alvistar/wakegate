// Command wakegate releases scheduling-gated pods onto an on-demand node that
// it wakes with Wake-on-LAN, or onto a fallback node when it does not wake.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/alvistar/wakegate/internal/controller"
	"github.com/alvistar/wakegate/internal/wake"
)

// version is set at build time with -ldflags "-X main.version=$(cat VERSION)".
var version = "dev"

func main() {
	var (
		cfg          controller.Config
		mac          string
		broadcast    string
		namespaces   string
		metricsAddr  string
		probeAddr    string
		printVersion bool
	)
	flag.StringVar(&cfg.GateName, "gate-name", "", "scheduling gate wakegate owns, e.g. example.com/wake (required)")
	flag.StringVar(&cfg.OnDemandNode, "ondemand-node", "", "node that sleeps and is woken for work (required)")
	flag.StringVar(&cfg.FallbackNode, "fallback-node", "", "always-on node used when the on-demand node cannot take a pod (required)")
	flag.DurationVar(&cfg.Timeout, "wake-timeout", 90*time.Second, "how long a pod waits for the on-demand node")
	flag.IntVar(&cfg.MaxPods, "max-ondemand-pods", 2, "concurrent pods allowed on the on-demand node (0 = unlimited)")
	flag.DurationVar(&cfg.PollInterval, "poll-interval", 2*time.Second, "re-evaluation and wake resend interval")
	flag.StringVar(&mac, "wol-mac", "", "MAC address of the on-demand node's wake-capable NIC (required)")
	flag.StringVar(&broadcast, "wol-broadcast", "", "broadcast host:port for the magic packet, e.g. 192.0.2.255:9 (required)")
	flag.StringVar(&namespaces, "namespaces", "", "comma-separated namespaces to watch; empty watches all")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":18080", "metrics endpoint; off the usual ports because wakegate runs on the host network")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":18081", "health probe endpoint")
	flag.BoolVar(&printVersion, "version", false, "print the version and exit")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if printVersion {
		fmt.Println(version)
		return
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	hw, err := net.ParseMAC(mac)
	if err != nil || broadcast == "" || cfg.GateName == "" || cfg.OnDemandNode == "" || cfg.FallbackNode == "" {
		setupLog.Error(err, "--gate-name, --ondemand-node, --fallback-node, --wol-mac and --wol-broadcast are required")
		os.Exit(2)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		setupLog.Error(err, "scheme")
		os.Exit(1)
	}
	cacheOpts := cache.Options{}
	if namespaces != "" {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{}
		for _, ns := range strings.Split(namespaces, ",") {
			cacheOpts.DefaultNamespaces[strings.TrimSpace(ns)] = cache.Config{}
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cacheOpts,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "manager")
		os.Exit(1)
	}

	r := &controller.PodReconciler{
		Client:   mgr.GetClient(),
		Waker:    wake.WoL{MAC: hw, Broadcast: broadcast},
		Recorder: mgr.GetEventRecorder("wakegate"),
		Config:   cfg,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "controller")
		os.Exit(1)
	}
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)

	setupLog.Info("starting wakegate", "version", version, "config", cfg, "wol-broadcast", broadcast)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}
