/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"sigs.k8s.io/dranet/pkg/driver"
	"sigs.k8s.io/dranet/pkg/features"
	"sigs.k8s.io/dranet/pkg/vfio"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"
)

const (
	driverName = "vfio.petasus.io"
)

var (
	hostnameOverride string
	kubeconfig       string
	bindAddress      string
	celExpression    string
	dbPath           string
	rescanInterval   time.Duration
	vfLockDir        string
	featureGates     string

	kubeletRootDir string

	ready atomic.Bool
)

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&bindAddress, "bind-address", ":9177", "The IP address and port for the metrics and healthz server to serve on")
	flag.StringVar(&hostnameOverride, "hostname-override", "", "If non-empty, will be used as the name of the Node that kube-network-policies is running on. If unset, the node name is assumed to be the same as the node's hostname.")
	flag.StringVar(&celExpression, "filter", "", "CEL expression to filter network interface attributes (v1.DeviceAttribute).")
	flag.StringVar(&dbPath, "db-path", filepath.Join("/var/run/dranet-vfio", "state.db"), "Path to the persistent bbolt database file. Set to an empty string to disable persistence and use in-memory state.")
	flag.DurationVar(&rescanInterval, "rescan-interval", 30*time.Second, "How often the host's vfio-pci bindings are rescanned and republished.")
	flag.StringVar(&vfLockDir, "vf-lock-dir", "/host/var/run", "Directory holding the per-bridge flock files serializing bridge-VLAN edits; point it at the host's /var/run so a host-installed sriov-vfio CNI shares the same locks.")
	flag.StringVar(&kubeletRootDir, "kubelet-root-dir", "/var/lib/kubelet", "The kubelet data directory (its --root-dir). The driver's registration socket lives under <dir>/plugins_registry and its dra.sock under <dir>/plugins/<driver-name>. Set this to match the kubelet --root-dir on clusters that relocate it.")
	flag.StringVar(&featureGates, "feature-gates", "", "A set of key=value pairs that describe feature gates for alpha/experimental features.")

	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: dranet-vfio [options]\n\n")
		flag.PrintDefaults()
	}
}

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	logs.GlogSetter(flag.Lookup("v").Value.String()) //nolint:errcheck

	if err := features.DefaultMutableFeatureGate.Set(featureGates); err != nil {
		klog.Fatalf("Error setting feature gates: %v", err)
	}

	printVersion()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Metrics and healthz server
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			if !ready.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		_ = http.ListenAndServe(bindAddress, mux)
	}()

	var config *rest.Config
	var err error
	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		klog.Fatalf("Failed to build kubernetes config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create kubernetes clientset: %v", err)
	}

	nodeName, err := nodeName(hostnameOverride)
	if err != nil {
		klog.Fatalf("Failed to get node name: %v", err)
	}

	opts := []driver.Option{
		driver.WithDBPath(dbPath),
		driver.WithKubeletRootDir(kubeletRootDir),
		driver.WithVFLockDir(vfLockDir),
	}
	if celExpression != "" {
		env, err := cel.NewEnv(
			cel.Variable("attributes", cel.MapType(cel.StringType, cel.DynType)),
			ext.NativeTypes(resourcev1.DeviceAttribute{}),
		)
		if err != nil {
			klog.Fatalf("failed to create CEL environment: %v", err)
		}
		ast, issues := env.Compile(celExpression)
		if issues != nil && issues.Err() != nil {
			klog.Fatalf("failed to compile CEL expression: %v", issues.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			klog.Fatalf("failed to create CEL program: %v", err)
		}
		opts = append(opts, driver.WithFilter(prg))
	}
	db := vfio.New(nodeName, vfio.WithRescanInterval(rescanInterval))
	opts = append(opts, driver.WithInventory(db))
	vfioDriver, err := driver.Start(ctx, driverName, clientset, nodeName, opts...)
	if err != nil {
		klog.Fatalf("driver failed to start: %v", err)
	}
	defer vfioDriver.Stop(cancel)

	ready.Store(true)
	klog.Info("driver started")

	<-ctx.Done()
	klog.Info("driver stopped")
}

func nodeName(hostnameOverride string) (string, error) {
	if hostnameOverride != "" {
		return hostnameOverride, nil
	}
	if name := os.Getenv("NODE_NAME"); name != "" {
		return name, nil
	}
	return os.Hostname()
}

func printVersion() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	var vcsRevision, vcsTime string
	for _, f := range info.Settings {
		switch f.Key {
		case "vcs.revision":
			vcsRevision = f.Value
		case "vcs.time":
			vcsTime = f.Value
		}
	}
	klog.Infof("dranet go %s build: %s time: %s", info.GoVersion, vcsRevision, vcsTime)
}
