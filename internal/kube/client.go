// Package kube builds Kubernetes clients for the two supported credential
// sources: the in-cluster ServiceAccount (primary) and a kubeconfig file
// (local/evaluation mode). Credential source is the only difference between
// the two deployment models.
package kube

import (
	"fmt"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// Clients bundles the typed core client and the metrics.k8s.io client.
type Clients struct {
	Core    kubernetes.Interface
	Metrics metricsclient.Interface
	// InCluster reports which credential source was used.
	InCluster bool
}

// New tries in-cluster config first, then falls back to the given kubeconfig
// path (or the default loading rules when the path is empty).
func New(kubeconfig string) (*Clients, error) {
	cfg, inCluster, err := restConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	// The collector is deliberately gentle (informers + one poll every ~15s),
	// but disk fetches fan out per node; give ourselves sane client-side limits.
	cfg.QPS = 20
	cfg.Burst = 40

	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building core client: %w", err)
	}
	metrics, err := metricsclient.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building metrics client: %w", err)
	}
	return &Clients{Core: core, Metrics: metrics, InCluster: inCluster}, nil
}

func restConfig(kubeconfig string) (*rest.Config, bool, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, true, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, false, fmt.Errorf("no in-cluster credentials and kubeconfig loading failed: %w", err)
	}
	fmt.Fprintln(os.Stderr, "kshows: using kubeconfig credentials (local mode); in-cluster is the primary deployment model")
	return cfg, false, nil
}
