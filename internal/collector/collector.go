// Package collector maintains the in-memory cluster model. Nodes and pods
// come from shared informer caches (cheap on large clusters, no re-listing);
// live usage is polled from the Metrics Server; node disk comes from the
// kubelet Summary API. Every poll interval it assembles an immutable
// model.Snapshot and hands it to subscribers.
package collector

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/tekikaito/kshows/internal/kube"
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

	mu       sync.RWMutex
	latest   *model.Snapshot
	subs     map[chan *model.Snapshot]struct{}
	diskByNode map[string]model.Disk
	diskLive   bool
	lastDisk   time.Time

	diskForbiddenOnce sync.Once
	logf              func(format string, args ...any)
}

// diskInterval is how often the per-node Summary API fan-out runs. Disk fills
// slowly; hitting every kubelet on the CPU/RAM cadence would be wasted load.
const diskInterval = 60 * time.Second

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
		logf:         log.Printf,
	}
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
	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		c.logf("listing nodes from cache: %v", err)
		return
	}
	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		c.logf("listing pods from cache: %v", err)
		return
	}

	podUsage, nodeUsage, metricsOK := c.fetchMetrics(ctx)
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
}

// fetchMetrics polls the Metrics Server. A failure is not an error condition:
// many clusters simply don't run it, so we degrade to requests/limits-only.
func (c *Collector) fetchMetrics(ctx context.Context) (map[string]model.Resources, map[string]model.Resources, bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	podUsage := map[string]model.Resources{}
	nodeUsage := map[string]model.Resources{}

	nodeMetrics, err := c.clients.Metrics.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return podUsage, nodeUsage, false
	}
	for _, nm := range nodeMetrics.Items {
		nodeUsage[nm.Name] = model.Resources{
			CPUMillis: nm.Usage.Cpu().MilliValue(),
			MemBytes:  nm.Usage.Memory().Value(),
		}
	}
	podMetrics, err := c.clients.Metrics.MetricsV1beta1().PodMetricses(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return podUsage, nodeUsage, false
	}
	for _, pm := range podMetrics.Items {
		var u model.Resources
		for _, cm := range pm.Containers {
			u.CPUMillis += cm.Usage.Cpu().MilliValue()
			u.MemBytes += cm.Usage.Memory().Value()
		}
		podUsage[pm.Namespace+"/"+pm.Name] = u
	}
	return podUsage, nodeUsage, true
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
	disk, ok := c.fetchDisk(ctx, names)
	c.mu.Lock()
	c.diskByNode = disk
	c.diskLive = ok
	c.lastDisk = time.Now()
	c.mu.Unlock()
	return disk, ok
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
