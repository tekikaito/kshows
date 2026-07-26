// Package collector maintains the in-memory cluster model. Nodes and pods
// come from shared informer caches (cheap on large clusters, no re-listing);
// live usage is polled from the Metrics Server; node disk comes from the
// kubelet Summary API. Every poll interval it assembles an immutable
// model.Snapshot and hands it to subscribers.
package collector

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/tekikaito/kshows/internal/kube"
	"github.com/tekikaito/kshows/internal/metrics"
	"github.com/tekikaito/kshows/internal/model"
)

// Source is what the HTTP server consumes: the latest snapshot plus a
// subscription for pushes. Both the real Collector and the mock implement it.
type Source interface {
	Latest() *model.Snapshot
	Subscribe() (ch <-chan *model.Snapshot, cancel func())
}

// Collector is the real, cluster-backed Source.
type Collector struct {
	clients      *kube.Clients
	pollInterval time.Duration

	factory    informers.SharedInformerFactory
	nodeLister corelisters.NodeLister
	podLister  corelisters.PodLister

	// diskFetch performs the kubelet Summary API fan-out. It is a function
	// field so tests can substitute it: the fake clientset cannot serve
	// CoreV1().RESTClient() calls. Production always uses (*Collector).fetchDisk.
	diskFetch func(ctx context.Context, names []string) (map[string]model.Disk, error)

	mu           sync.RWMutex
	latest       *model.Snapshot
	subs         map[chan *model.Snapshot]struct{}
	diskByNode   map[string]model.Disk
	diskLive     bool
	diskFailures int
	lastDisk     time.Time

	// Metrics hysteresis state; only the poll goroutine touches these.
	metricsUp       bool
	metricsFailures int
	lastPodUsage    map[string]model.Resources
	lastNodeUsage   map[string]model.Resources

	diskForbiddenOnce sync.Once
	logf              func(format string, args ...any)
}

// diskInterval is how often the per-node Summary API fan-out runs. Disk fills
// slowly; hitting every kubelet on the CPU/RAM cadence would be wasted load.
const diskInterval = 60 * time.Second

// capFailThreshold is how many consecutive transient failures it takes to
// drop a capability. One apiserver blip must not flap the UI banner.
const capFailThreshold = 3

func New(clients *kube.Clients, pollInterval time.Duration) *Collector {
	factory := informers.NewSharedInformerFactory(clients.Core, 10*time.Minute)
	c := &Collector{
		clients:      clients,
		pollInterval: pollInterval,
		factory:      factory,
		nodeLister:   factory.Core().V1().Nodes().Lister(),
		podLister:    factory.Core().V1().Pods().Lister(),
		subs:         make(map[chan *model.Snapshot]struct{}),
		diskByNode:   map[string]model.Disk{},
		// Start optimistic: a definitive absence or a failure streak flips
		// these with one transition log, instead of a spurious "restored"
		// on the first successful poll.
		metricsUp: true,
		diskLive:  true,
		logf:      log.Printf,
	}
	c.diskFetch = c.fetchDisk
	return c
}

// Run starts the informers, waits for their caches, then polls until ctx ends.
func (c *Collector) Run(ctx context.Context) error {
	nodeInformer := c.factory.Core().V1().Nodes().Informer()
	podInformer := c.factory.Core().V1().Pods().Informer()
	c.factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), nodeInformer.HasSynced, podInformer.HasSynced) {
		return ctx.Err()
	}
	c.logf("informer caches synced; polling every %s", c.pollInterval)

	c.poll(ctx)
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.poll(ctx)
		}
	}
}

func (c *Collector) Latest() *model.Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latest
}

func (c *Collector) Subscribe() (<-chan *model.Snapshot, func()) {
	ch := make(chan *model.Snapshot, 1)
	c.mu.Lock()
	c.subs[ch] = struct{}{}
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		delete(c.subs, ch)
		c.mu.Unlock()
	}
}

func (c *Collector) publish(snap *model.Snapshot) {
	c.mu.Lock()
	c.latest = snap
	for ch := range c.subs {
		// Non-blocking with capacity 1: a slow SSE client skips to the next
		// snapshot instead of stalling the poll loop.
		select {
		case ch <- snap:
		default:
		}
	}
	c.mu.Unlock()
}

func (c *Collector) poll(ctx context.Context) {
	start := time.Now()
	err := c.pollOnce(ctx)
	metrics.ObservePoll(time.Since(start), err)
	if err != nil {
		c.logf("poll failed: %v", err)
	}
}

// pollOnce assembles and publishes one snapshot. A returned error means no
// snapshot was published at all; a degraded optional signal is not an error.
func (c *Collector) pollOnce(ctx context.Context) error {
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("listing nodes from cache: %w", err)
	}
	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("listing pods from cache: %w", err)
	}

	podUsage, nodeUsage, metricsErr := c.fetchMetrics(ctx)
	podUsage, nodeUsage, metricsOK := c.noteMetrics(podUsage, nodeUsage, metricsErr)
	diskByNode, diskLive := c.currentDisk(ctx, nodes)

	// Group scheduled, non-terminal pods by node.
	podsByNode := make(map[string][]model.Pod, len(nodes))
	for _, p := range pods {
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		requests, limits := effectiveResources(&p.Spec)
		mp := model.Pod{
			UID:       string(p.UID),
			Name:      p.Name,
			Namespace: p.Namespace,
			Requests:  requests,
			Limits:    limits,
		}
		if u, ok := podUsage[p.Namespace+"/"+p.Name]; ok {
			mp.Usage = u
			mp.HasUsage = true
		}
		podsByNode[p.Spec.NodeName] = append(podsByNode[p.Spec.NodeName], mp)
	}

	snap := &model.Snapshot{
		GeneratedAt: time.Now().UTC(),
		Nodes:       make([]model.Node, 0, len(nodes)),
		Capabilities: model.Capabilities{
			Metrics: metricsOK,
			Disk:    diskLive,
		},
	}
	for _, n := range nodes {
		mn := model.Node{
			Name:  n.Name,
			Ready: nodeReady(n),
			Roles: nodeRoles(n),
			Allocatable: model.Allocatable{
				CPUMillis: n.Status.Allocatable.Cpu().MilliValue(),
				MemBytes:  n.Status.Allocatable.Memory().Value(),
				DiskBytes: quantityValue(n.Status.Allocatable, corev1.ResourceEphemeralStorage),
				Pods:      quantityValue(n.Status.Allocatable, corev1.ResourcePods),
			},
			Pods: podsByNode[n.Name],
		}
		if u, ok := nodeUsage[n.Name]; ok {
			mn.Usage = u
			mn.HasUsage = true
		}
		if d, ok := diskByNode[n.Name]; ok {
			mn.Disk = d
		} else {
			// Fallback: capacity from allocatable, no live used figure.
			mn.Disk = model.Disk{CapacityBytes: mn.Allocatable.DiskBytes, Live: false}
		}
		sort.Slice(mn.Pods, func(i, j int) bool { return mn.Pods[i].UID < mn.Pods[j].UID })
		snap.Nodes = append(snap.Nodes, mn)
	}
	sort.Slice(snap.Nodes, func(i, j int) bool { return snap.Nodes[i].Name < snap.Nodes[j].Name })

	c.publish(snap)
	return nil
}

// fetchMetrics polls the Metrics Server. A failure is not an error condition:
// many clusters simply don't run it, so we degrade to requests/limits-only.
// The error is returned raw so noteMetrics can classify it.
func (c *Collector) fetchMetrics(ctx context.Context) (map[string]model.Resources, map[string]model.Resources, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	podUsage := map[string]model.Resources{}
	nodeUsage := map[string]model.Resources{}

	nodeMetrics, err := c.clients.Metrics.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return podUsage, nodeUsage, err
	}
	for _, nm := range nodeMetrics.Items {
		nodeUsage[nm.Name] = model.Resources{
			CPUMillis: nm.Usage.Cpu().MilliValue(),
			MemBytes:  nm.Usage.Memory().Value(),
		}
	}
	podMetrics, err := c.clients.Metrics.MetricsV1beta1().PodMetricses(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return podUsage, nodeUsage, err
	}
	for _, pm := range podMetrics.Items {
		var u model.Resources
		for _, cm := range pm.Containers {
			u.CPUMillis += cm.Usage.Cpu().MilliValue()
			u.MemBytes += cm.Usage.Memory().Value()
		}
		podUsage[pm.Namespace+"/"+pm.Name] = u
	}
	return podUsage, nodeUsage, nil
}

// metricsAbsent reports whether err means the metrics.k8s.io API group is not
// installed at all, as opposed to a transient failure reaching it.
func metricsAbsent(err error) bool {
	return errors.IsNotFound(err) || meta.IsNoMatchError(err)
}

// noteMetrics folds one fetch result into the metrics capability state and
// returns the usage maps to build the snapshot from. Definitive absence drops
// the capability immediately; transient errors keep the last-known usage and
// the previous capability until capFailThreshold consecutive failures. Only
// called from the poll goroutine.
func (c *Collector) noteMetrics(podUsage, nodeUsage map[string]model.Resources, err error) (map[string]model.Resources, map[string]model.Resources, bool) {
	switch {
	case err == nil:
		metrics.RecordSignal(metrics.SignalMetrics, metrics.ResultSuccess)
		if !c.metricsUp {
			c.logf("metrics capability restored")
		}
		c.metricsUp = true
		c.metricsFailures = 0
		c.lastPodUsage, c.lastNodeUsage = podUsage, nodeUsage
	case metricsAbsent(err):
		metrics.RecordSignal(metrics.SignalMetrics, metrics.ResultAbsent)
		if c.metricsUp {
			c.logf("metrics.k8s.io not available: %v (degrading to requests/limits-only)", err)
		}
		c.metricsUp = false
		c.metricsFailures = 0
		c.lastPodUsage, c.lastNodeUsage = nil, nil
	default:
		metrics.RecordSignal(metrics.SignalMetrics, metrics.ResultError)
		c.metricsFailures++
		if c.metricsUp && c.metricsFailures >= capFailThreshold {
			c.logf("metrics capability lost after %d consecutive failures: %v", c.metricsFailures, err)
			c.metricsUp = false
			c.lastPodUsage, c.lastNodeUsage = nil, nil
		}
	}
	return c.lastPodUsage, c.lastNodeUsage, c.metricsUp
}

// currentDisk returns cached Summary API results, refreshing them on the
// slower diskInterval cadence.
func (c *Collector) currentDisk(ctx context.Context, nodes []*corev1.Node) (map[string]model.Disk, bool) {
	c.mu.RLock()
	fresh := time.Since(c.lastDisk) < diskInterval
	cached, live := c.diskByNode, c.diskLive
	c.mu.RUnlock()
	if fresh {
		return cached, live
	}

	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	disk, err := c.diskFetch(ctx, names)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastDisk = time.Now()
	switch {
	case err == nil:
		metrics.RecordSignal(metrics.SignalDisk, metrics.ResultSuccess)
		if !c.diskLive {
			c.logf("disk capability restored")
		}
		c.diskByNode = disk
		c.diskLive = true
		c.diskFailures = 0
	case errors.IsForbidden(err):
		metrics.RecordSignal(metrics.SignalDisk, metrics.ResultAbsent)
		// Definitive: RBAC won't heal on its own, so no hysteresis and no
		// stale cache to serve. noteDiskForbidden already logged the hint.
		c.diskByNode = disk
		c.diskLive = false
		c.diskFailures = 0
	default:
		metrics.RecordSignal(metrics.SignalDisk, metrics.ResultError)
		// Transient: keep the cached per-node data and the previous
		// capability until the failure streak proves the signal is gone.
		c.diskFailures++
		if c.diskLive && c.diskFailures >= capFailThreshold {
			c.logf("disk capability lost after %d consecutive refresh failures: %v", c.diskFailures, err)
			c.diskLive = false
			// Drop the stale per-node data along with the capability: serving
			// Live disk figures while the capability banner says "no disk
			// signal" would contradict itself. Nodes fall back to
			// capacity-only until a fetch succeeds again.
			c.diskByNode = map[string]model.Disk{}
		}
	}
	return c.diskByNode, c.diskLive
}

func nodeReady(n *corev1.Node) bool {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func nodeRoles(n *corev1.Node) []string {
	var roles []string
	for label := range n.Labels {
		if role, ok := strings.CutPrefix(label, "node-role.kubernetes.io/"); ok && role != "" {
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	return roles
}
