// Command agent runs on every Raspberry Pi node. It reports the node's
// bootloader state and, only when the controller approves, stages an EEPROM
// update and reboots.
package main

import (
	"flag"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	rpiv1 "github.com/conallob/raspberrypi-operator/api/v1alpha1"
	"github.com/conallob/raspberrypi-operator/internal/agent"
	"github.com/conallob/raspberrypi-operator/internal/node"
)

func main() {
	var nodeName, bootfs, probeAddr string
	var interval time.Duration
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "name of the node this agent runs on")
	flag.StringVar(&bootfs, "boot-fs", "/host/boot/firmware", "where the host's boot partition is mounted")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the probe endpoint binds to")
	flag.DurationVar(&interval, "interval", 30*time.Second, "how often to re-probe the node")
	zopts := zap.Options{}
	zopts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zopts)))
	log := ctrl.Log.WithName("setup")

	if nodeName == "" {
		log.Error(nil, "--node-name or NODE_NAME is required")
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(rpiv1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		// Only ever see this node's object.
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&rpiv1.NodeBootloader{}: {Field: fields.OneTermEqualSelector("metadata.name", nodeName)},
		}},
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	prober := node.Prober{Root: "/", BootFS: bootfs}
	r := &agent.Reconciler{
		Client:   mgr.GetClient(),
		NodeName: nodeName,
		BootFS:   bootfs,
		Probe:    prober.Probe,
		Fetch:    agent.HTTPFetcher{Client: &http.Client{Timeout: 2 * time.Minute}},
		Rebooter: node.SyscallRebooter{},
		Now:      time.Now,
		Interval: interval,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller")
		os.Exit(1)
	}
	utilruntime.Must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	utilruntime.Must(mgr.AddReadyzCheck("readyz", healthz.Ping))

	log.Info("starting agent", "node", nodeName, "bootfs", bootfs)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "agent exited")
		os.Exit(1)
	}
}
