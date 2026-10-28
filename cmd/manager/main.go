package main

import (
	"flag"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kylape/sno-vm-operator/internal/cache"
)

func main() {
	var path, healthAddress string
	var interval time.Duration
	flag.StringVar(&path, "config", "/etc/sno-operator/config.yaml", "Administrator cache policy")
	flag.StringVar(&healthAddress, "health-address", ":8081", "Health probe address")
	flag.DurationVar(&interval, "interval", 15*time.Second, "Cache reconciliation interval")
	options := zap.Options{}
	options.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&options)))
	logger := ctrl.Log.WithName("manager")
	policy, err := cache.Load(path)
	if err != nil {
		logger.Error(err, "invalid configuration")
		os.Exit(1)
	}
	if interval < time.Second {
		logger.Error(nil, "interval must be at least one second")
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	if err = corev1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err = storagev1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	kubeconfig := ctrl.GetConfigOrDie()
	manager, err := ctrl.NewManager(kubeconfig, ctrl.Options{
		Scheme:         scheme,
		LeaderElection: true, LeaderElectionID: "sno-vm-operator.sno.klape.io", LeaderElectionNamespace: policy.Namespace,
		Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: healthAddress,
	})
	if err != nil {
		logger.Error(err, "create manager")
		os.Exit(1)
	}
	// Direct API reads keep the retry ledger and CDI/PVC observations current.
	// No broad Secret informer or cluster-wide Secret list permission is needed.
	apiClient, err := client.New(kubeconfig, client.Options{Scheme: scheme})
	if err != nil {
		logger.Error(err, "create API client")
		os.Exit(1)
	}
	if err = manager.Add(&cache.Reconciler{Client: apiClient, Config: policy, Interval: interval}); err != nil {
		panic(err)
	}
	if err = manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		panic(err)
	}
	if err = manager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		panic(err)
	}
	logger.Info("starting cache controller; instance provisioning is not enabled")
	if err = manager.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "manager stopped")
		os.Exit(1)
	}
}
