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

	// Switches for the two optional signals. Off means never requested.
	metricsEnabled bool
	diskEnabled    bool

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
	diskReason   string
	diskFailures int
	lastDisk     time.Time

	// Metrics hysteresis state; only the poll goroutine touches these.
	metricsUp       bool
	metricsReason   string
	metricsFailures int
	metricsRetryAt  time.Time // zero unless parked after a 403
	lastPodUsage    map[string]model.Resources
	lastNodeUsage   map[string]model.Resources

	logf func(format string, args ...any)
}

// Options configures a Collector.
type Options struct {
	// PollInterval is how often a snapshot is assembled.
	PollInterval time.Duration
	// MetricsServer and NodeDisk switch the two optional signals. Off means
	// the signal is never requested, so a cluster that withholds the
	// permission on purpose sees no denied requests from kshows at all.
	MetricsServer bool
	NodeDisk      bool
}

// diskInterval is how often the per-node Summary API fan-out runs. Disk fills
// slowly; hitting every kubelet on the CPU/RAM cadence would be wasted load.
const diskInterval = 60 * time.Second

// capFailThreshold is how many consecutive transient failures it takes to
// drop a capability. One apiserver blip must not flap the UI banner.
const capFailThreshold = 3

// forbiddenRetry is how long a signal stays parked after the API server
// answers 403. A permission can be granted later, so kshows asks again, but
// rarely: every denied request is an entry in the cluster's audit log, and an
// admin who withheld the permission on purpose should not see one per poll.
const forbiddenRetry = 10 * time.Minute

func New(clients *kube.Clients, opts Options) *Collector {
	factory := informers.NewSharedInformerFactory(clients.Core, 10*time.Minute)
	c := &Collector{
		clients:        clients,
		pollInterval:   opts.PollInterval,
		metricsEnabled: opts.MetricsServer,
		diskEnabled:    opts.NodeDisk,
		factory:        factory,
		nodeLister:     factory.Core().V1().Nodes().Lister(),
		podLister:      factory.Core().V1().Pods().Lister(),
		subs:           make(map[chan *model.Snapshot]struct{}),
		diskByNode:     map[string]model.Disk{},
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
	if !c.metricsEnabled {
		c.logf("live CPU/RAM usage disabled (--metrics-server=false): showing requests/limits only")
	}
	if !c.diskEnabled {
		c.logf("live node disk disabled (--node-disk=false): disk shows capacity only")
	}

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

	podUsage, nodeUsage, metricsOK, metricsReason := c.currentMetrics(ctx)
	diskByNode, diskLive, diskReason := c.currentDisk(ctx, nodes)

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
			Metrics:       metricsOK,
			MetricsReason: metricsReason,
			Disk:          diskLive,
			DiskReason:    diskReason,
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

// currentMetrics returns the usage maps to build the snapshot from. It does
// not touch the Metrics Server while the signal is disabled or parked after a
// 403. Only called from the poll goroutine.
func (c *Collector) currentMetrics(ctx context.Context) (map[string]model.Resources, map[string]model.Resources, bool, string) {
	if !c.metricsEnabled {
		return nil, nil, false, model.ReasonDisabled
	}
	if time.Now().Before(c.metricsRetryAt) {
		return c.lastPodUsage, c.lastNodeUsage, c.metricsUp, c.metricsReason
	}
	podUsage, nodeUsage, err := c.fetchMetrics(ctx)
	return c.noteMetrics(podUsage, nodeUsage, err)
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
// returns the usage maps to build the snapshot from. Definitive absence or a
// 403 drops the capability immediately (a 403 also parks the signal for
// forbiddenRetry); transient errors keep the last-known usage and the
// previous capability until capFailThreshold consecutive failures. Only
// called from the poll goroutine.
func (c *Collector) noteMetrics(podUsage, nodeUsage map[string]model.Resources, err error) (map[string]model.Resources, map[string]model.Resources, bool, string) {
	switch {
	case err == nil:
		metrics.RecordSignal(metrics.SignalMetrics, metrics.ResultSuccess)
		if !c.metricsUp {
			c.logf("metrics capability restored")
		}
		c.metricsUp = true
		c.metricsReason = ""
		c.metricsFailures = 0
		c.lastPodUsage, c.lastNodeUsage = podUsage, nodeUsage
	case errors.IsForbidden(err):
		metrics.RecordSignal(metrics.SignalMetrics, metrics.ResultAbsent)
		if c.metricsReason != model.ReasonForbidden {
			c.logf("metrics.k8s.io is forbidden: showing requests/limits only; asking again every %s "+
				"(grant get,list on metrics.k8s.io nodes and pods, or run with --metrics-server=false to stop asking)", forbiddenRetry)
		}
		c.metricsUp = false
		c.metricsReason = model.ReasonForbidden
		c.metricsFailures = 0
		c.metricsRetryAt = time.Now().Add(forbiddenRetry)
		c.lastPodUsage, c.lastNodeUsage = nil, nil
	case metricsAbsent(err):
		metrics.RecordSignal(metrics.SignalMetrics, metrics.ResultAbsent)
		if c.metricsReason != model.ReasonAbsent {
			c.logf("metrics.k8s.io not available: %v (degrading to requests/limits-only)", err)
		}
		c.metricsUp = false
		c.metricsReason = model.ReasonAbsent
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
		if !c.metricsUp {
			c.metricsReason = model.ReasonUnavailable
		}
	}
	return c.lastPodUsage, c.lastNodeUsage, c.metricsUp, c.metricsReason
}

// currentDisk returns cached Summary API results, refreshing them on the
// slower diskInterval cadence, or on forbiddenRetry after a 403. It never
// fetches while the signal is disabled.
func (c *Collector) currentDisk(ctx context.Context, nodes []*corev1.Node) (map[string]model.Disk, bool, string) {
	if !c.diskEnabled {
		return nil, false, model.ReasonDisabled
	}
	c.mu.RLock()
	wait := diskInterval
	if c.diskReason == model.ReasonForbidden {
		wait = forbiddenRetry
	}
	fresh := time.Since(c.lastDisk) < wait
	cached, live, reason := c.diskByNode, c.diskLive, c.diskReason
	c.mu.RUnlock()
	if fresh {
		return cached, live, reason
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
		c.diskReason = ""
		c.diskFailures = 0
	case errors.IsForbidden(err):
		metrics.RecordSignal(metrics.SignalDisk, metrics.ResultAbsent)
		// Definitive until someone changes RBAC: no hysteresis, no stale
		// cache to serve, and the next attempt waits forbiddenRetry.
		if c.diskReason != model.ReasonForbidden {
			c.logf("nodes/proxy is forbidden: disk shows capacity only; asking again every %s "+
				"(grant get on nodes/proxy for live usage, or run with --node-disk=false to stop asking)", forbiddenRetry)
		}
		c.diskByNode = disk
		c.diskLive = false
		c.diskReason = model.ReasonForbidden
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
		if !c.diskLive {
			c.diskReason = model.ReasonUnavailable
		}
	}
	return c.diskByNode, c.diskLive, c.diskReason
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
