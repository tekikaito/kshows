package collector

import (
	"context"
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	corefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"

	"github.com/tekikaito/kshows/internal/kube"
	"github.com/tekikaito/kshows/internal/model"
)

// --- fixtures ---------------------------------------------------------------

// allocList extends rl (from resources_test.go) with the node-only resources.
func allocList(cpu, mem, disk, pods string) corev1.ResourceList {
	out := rl(cpu, mem)
	out[corev1.ResourceEphemeralStorage] = resource.MustParse(disk)
	out[corev1.ResourcePods] = resource.MustParse(pods)
	return out
}

func testNode(name string, ready bool, labels map[string]string, alloc corev1.ResourceList) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Allocatable: alloc,
			Conditions: []corev1.NodeCondition{
				// A non-Ready condition first, so the lookup has to find NodeReady.
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
				{Type: corev1.NodeReady, Status: status},
			},
		},
	}
}

func testPod(ns, name, uid, nodeName string, phase corev1.PodPhase, requests corev1.ResourceList) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{Name: "main", Resources: corev1.ResourceRequirements{Requests: requests}},
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func testNodeMetrics(name string, usage corev1.ResourceList) *metricsv1beta1.NodeMetrics {
	return &metricsv1beta1.NodeMetrics{ObjectMeta: metav1.ObjectMeta{Name: name}, Usage: usage}
}

func testPodMetrics(ns, name string, containerUsage ...corev1.ResourceList) *metricsv1beta1.PodMetrics {
	pm := &metricsv1beta1.PodMetrics{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	for i, u := range containerUsage {
		pm.Containers = append(pm.Containers, metricsv1beta1.ContainerMetrics{
			Name: fmt.Sprintf("c%d", i), Usage: u,
		})
	}
	return pm
}

// seedMetrics puts metrics fixtures into the fake tracker under the GVRs the
// generated fake client actually lists ("nodes"/"pods" in metrics.k8s.io, not
// the "nodemetricses"/"podmetricses" the tracker would guess from the kind —
// objects passed to NewSimpleClientset are silently never returned).
func seedMetrics(t *testing.T, metrics *metricsfake.Clientset, objs []runtime.Object) {
	t.Helper()
	for _, obj := range objs {
		var err error
		switch o := obj.(type) {
		case *metricsv1beta1.NodeMetrics:
			err = metrics.Tracker().Create(metricsv1beta1.SchemeGroupVersion.WithResource("nodes"), o, "")
		case *metricsv1beta1.PodMetrics:
			err = metrics.Tracker().Create(metricsv1beta1.SchemeGroupVersion.WithResource("pods"), o, o.Namespace)
		default:
			t.Fatalf("unsupported metrics fixture type %T", obj)
		}
		if err != nil {
			t.Fatalf("seeding metrics fixture: %v", err)
		}
	}
}

// newTestCollector wires a Collector to fake clientsets, stubs the disk fetch
// (the fake core client cannot serve RESTClient() calls) and syncs the
// informer caches so poll() can be driven directly.
func newTestCollector(t *testing.T, coreObjs, metricsObjs []runtime.Object) (*Collector, *metricsfake.Clientset) {
	t.Helper()
	core := corefake.NewSimpleClientset(coreObjs...)
	metrics := metricsfake.NewSimpleClientset()
	seedMetrics(t, metrics, metricsObjs)
	c := New(&kube.Clients{Core: core, Metrics: metrics}, time.Second)
	c.logf = t.Logf
	c.diskFetch = func(ctx context.Context, names []string) (map[string]model.Disk, error) {
		return map[string]model.Disk{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.factory.Start(ctx.Done())
	for typ, ok := range c.factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			t.Fatalf("informer cache sync failed for %v", typ)
		}
	}
	return c, metrics
}

// forceDiskRefresh rewinds the disk cache timestamp so the next poll refetches
// instead of hitting the 60s diskInterval gate.
func forceDiskRefresh(c *Collector) {
	c.mu.Lock()
	c.lastDisk = time.Time{}
	c.mu.Unlock()
}

// --- snapshot assembly -------------------------------------------------------

func TestPollSnapshotAssembly(t *testing.T) {
	coreObjs := []runtime.Object{
		// Created out of name order to prove sorting.
		testNode("node-b", true, map[string]string{
			"node-role.kubernetes.io/worker": "",
			"node-role.kubernetes.io/app":    "",
			"node-role.kubernetes.io/":       "", // empty role: must be ignored
			"kubernetes.io/hostname":         "node-b",
		}, allocList("4", "16Gi", "100Gi", "110")),
		testNode("node-a", false, nil, allocList("2", "8Gi", "50Gi", "58")),

		// UIDs out of order to prove per-node pod sorting.
		testPod("default", "p1", "uid-b", "node-a", corev1.PodRunning, rl("500m", "512Mi")),
		testPod("default", "p0", "uid-a", "node-a", corev1.PodPending, nil),
		testPod("default", "done", "uid-c", "node-a", corev1.PodSucceeded, rl("1", "1Gi")),
		testPod("default", "crashed", "uid-d", "node-b", corev1.PodFailed, rl("1", "1Gi")),
		testPod("default", "unscheduled", "uid-e", "", corev1.PodRunning, rl("1", "1Gi")),
		testPod("kube-system", "kb", "uid-f", "node-b", corev1.PodRunning, rl("100m", "128Mi")),
	}
	metricsObjs := []runtime.Object{
		testNodeMetrics("node-a", rl("1500m", "4Gi")),
		// Usage is summed across containers: 100+150 = 250m, 10+20 = 30Mi.
		testPodMetrics("default", "p1", rl("100m", "10Mi"), rl("150m", "20Mi")),
	}
	c, _ := newTestCollector(t, coreObjs, metricsObjs)

	liveDisk := model.Disk{CapacityBytes: 111, UsedBytes: 42, AvailableBytes: 69, Live: true}
	c.diskFetch = func(ctx context.Context, names []string) (map[string]model.Disk, error) {
		return map[string]model.Disk{"node-a": liveDisk}, nil
	}

	c.poll(context.Background())
	snap := c.Latest()
	if snap == nil {
		t.Fatal("Latest() returned nil after poll")
	}

	t.Run("capabilities", func(t *testing.T) {
		if !snap.Capabilities.Metrics || !snap.Capabilities.Disk {
			t.Errorf("capabilities = %+v, want both true", snap.Capabilities)
		}
	})

	if len(snap.Nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(snap.Nodes))
	}
	na, nb := snap.Nodes[0], snap.Nodes[1]

	t.Run("nodes sorted by name", func(t *testing.T) {
		if na.Name != "node-a" || nb.Name != "node-b" {
			t.Errorf("node order = [%s, %s], want [node-a, node-b]", na.Name, nb.Name)
		}
	})

	t.Run("node fields", func(t *testing.T) {
		if na.Ready {
			t.Error("node-a Ready = true, want false")
		}
		if !nb.Ready {
			t.Error("node-b Ready = false, want true")
		}
		if len(na.Roles) != 0 {
			t.Errorf("node-a roles = %v, want none", na.Roles)
		}
		if len(nb.Roles) != 2 || nb.Roles[0] != "app" || nb.Roles[1] != "worker" {
			t.Errorf("node-b roles = %v, want [app worker]", nb.Roles)
		}
		want := model.Allocatable{CPUMillis: 2000, MemBytes: 8 << 30, DiskBytes: 50 << 30, Pods: 58}
		if na.Allocatable != want {
			t.Errorf("node-a allocatable = %+v, want %+v", na.Allocatable, want)
		}
	})

	t.Run("node usage joined by name", func(t *testing.T) {
		if !na.HasUsage {
			t.Fatal("node-a HasUsage = false, want true")
		}
		if want := (model.Resources{CPUMillis: 1500, MemBytes: 4 << 30}); na.Usage != want {
			t.Errorf("node-a usage = %+v, want %+v", na.Usage, want)
		}
		if nb.HasUsage {
			t.Error("node-b HasUsage = true, want false (no NodeMetrics for it)")
		}
	})

	t.Run("terminal and unscheduled pods excluded", func(t *testing.T) {
		if got := len(na.Pods); got != 2 {
			t.Fatalf("node-a has %d pods, want 2 (Succeeded pod excluded)", got)
		}
		if got := len(nb.Pods); got != 1 {
			t.Fatalf("node-b has %d pods, want 1 (Failed pod excluded)", got)
		}
		for _, p := range append(append([]model.Pod{}, na.Pods...), nb.Pods...) {
			switch p.Name {
			case "done", "crashed", "unscheduled":
				t.Errorf("pod %s should have been excluded", p.Name)
			}
		}
	})

	t.Run("pods sorted by UID within node", func(t *testing.T) {
		if na.Pods[0].UID != "uid-a" || na.Pods[1].UID != "uid-b" {
			t.Errorf("node-a pod UIDs = [%s, %s], want [uid-a, uid-b]", na.Pods[0].UID, na.Pods[1].UID)
		}
	})

	t.Run("pod requests wired through effectiveResources", func(t *testing.T) {
		p1 := na.Pods[1]
		if want := (model.Resources{CPUMillis: 500, MemBytes: 512 << 20}); p1.Requests != want {
			t.Errorf("p1 requests = %+v, want %+v", p1.Requests, want)
		}
	})

	t.Run("pod usage joined by namespace/name and summed", func(t *testing.T) {
		p1 := na.Pods[1]
		if !p1.HasUsage {
			t.Fatal("p1 HasUsage = false, want true")
		}
		if want := (model.Resources{CPUMillis: 250, MemBytes: 30 << 20}); p1.Usage != want {
			t.Errorf("p1 usage = %+v, want %+v", p1.Usage, want)
		}
		if na.Pods[0].HasUsage {
			t.Error("p0 HasUsage = true, want false (no PodMetrics for it)")
		}
	})

	t.Run("disk join and capacity-only fallback", func(t *testing.T) {
		if na.Disk != liveDisk {
			t.Errorf("node-a disk = %+v, want fetched %+v", na.Disk, liveDisk)
		}
		want := model.Disk{CapacityBytes: 100 << 30, Live: false}
		if nb.Disk != want {
			t.Errorf("node-b disk = %+v, want fallback %+v", nb.Disk, want)
		}
	})
}

// --- metrics capability hysteresis --------------------------------------------

// hysteresisFixtures is a one-node one-pod cluster with matching metrics, small
// enough that HasUsage flags are the only signal that matters.
func hysteresisFixtures() (coreObjs, metricsObjs []runtime.Object) {
	coreObjs = []runtime.Object{
		testNode("n1", true, nil, allocList("4", "16Gi", "50Gi", "110")),
		testPod("default", "p1", "uid-1", "n1", corev1.PodRunning, rl("500m", "512Mi")),
	}
	metricsObjs = []runtime.Object{
		testNodeMetrics("n1", rl("1", "2Gi")),
		testPodMetrics("default", "p1", rl("200m", "256Mi")),
	}
	return coreObjs, metricsObjs
}

// injectMetricsError makes every metrics list call fail with *errp while it is
// non-nil, and fall through to the fake's fixtures when it is nil.
func injectMetricsError(metrics *metricsfake.Clientset, errp *error) {
	metrics.PrependReactor("list", "*", func(ktesting.Action) (bool, runtime.Object, error) {
		if *errp != nil {
			return true, nil, *errp
		}
		return false, nil, nil
	})
}

func TestMetricsCapabilityHysteresis(t *testing.T) {
	coreObjs, metricsObjs := hysteresisFixtures()
	c, metrics := newTestCollector(t, coreObjs, metricsObjs)

	var injected error
	injectMetricsError(metrics, &injected)
	transient := apierrors.NewInternalError(stderrors.New("apiserver hiccup"))

	// Order-dependent sequence: each step is one poll.
	steps := []struct {
		name      string
		err       error
		wantCap   bool
		wantUsage bool // node and pod usage present in the snapshot
	}{
		{"initial success populates usage", nil, true, true},
		{"transient failure 1 keeps capability and cached usage", transient, true, true},
		{"transient failure 2 keeps capability and cached usage", transient, true, true},
		{"transient failure 3 drops capability and cached usage", transient, false, false},
		{"failure 4 stays down", transient, false, false},
		{"success restores immediately", nil, true, true},
		{"new streak failure 1", transient, true, true},
		{"new streak failure 2", transient, true, true},
		{"success mid-streak resets the counter", nil, true, true},
		{"post-reset failure 1", transient, true, true},
		{"post-reset failure 2 (would flip without the reset)", transient, true, true},
		{"post-reset failure 3 flips", transient, false, false},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			injected = step.err
			c.poll(context.Background())
			snap := c.Latest()
			if snap.Capabilities.Metrics != step.wantCap {
				t.Errorf("Capabilities.Metrics = %v, want %v", snap.Capabilities.Metrics, step.wantCap)
			}
			n := snap.Nodes[0]
			if n.HasUsage != step.wantUsage {
				t.Errorf("node HasUsage = %v, want %v", n.HasUsage, step.wantUsage)
			}
			if got := n.Pods[0].HasUsage; got != step.wantUsage {
				t.Errorf("pod HasUsage = %v, want %v", got, step.wantUsage)
			}
			if step.wantUsage {
				if want := (model.Resources{CPUMillis: 200, MemBytes: 256 << 20}); n.Pods[0].Usage != want {
					t.Errorf("pod usage = %+v, want %+v", n.Pods[0].Usage, want)
				}
			}
		})
	}
}

func TestMetricsAbsentDropsImmediately(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "metrics.k8s.io", Resource: "nodemetricses"}, "")

	t.Run("absent on the very first poll", func(t *testing.T) {
		coreObjs, metricsObjs := hysteresisFixtures()
		c, metrics := newTestCollector(t, coreObjs, metricsObjs)
		var injected error
		injectMetricsError(metrics, &injected)

		injected = notFound
		c.poll(context.Background())
		snap := c.Latest()
		if snap.Capabilities.Metrics {
			t.Error("Capabilities.Metrics = true, want false despite the optimistic start")
		}
		if snap.Nodes[0].HasUsage || snap.Nodes[0].Pods[0].HasUsage {
			t.Error("usage present, want none")
		}
	})

	t.Run("absent after a success drops without hysteresis", func(t *testing.T) {
		coreObjs, metricsObjs := hysteresisFixtures()
		c, metrics := newTestCollector(t, coreObjs, metricsObjs)
		var injected error
		injectMetricsError(metrics, &injected)

		c.poll(context.Background())
		if !c.Latest().Capabilities.Metrics {
			t.Fatal("setup: first poll should succeed")
		}

		injected = notFound
		c.poll(context.Background())
		snap := c.Latest()
		if snap.Capabilities.Metrics {
			t.Error("Capabilities.Metrics = true, want false on the same poll (definitive absence)")
		}
		if snap.Nodes[0].HasUsage || snap.Nodes[0].Pods[0].HasUsage {
			t.Error("cached usage still served, want it dropped for definitive absence")
		}

		injected = nil
		c.poll(context.Background())
		if snap := c.Latest(); !snap.Capabilities.Metrics || !snap.Nodes[0].Pods[0].HasUsage {
			t.Error("success after absence should restore capability and usage immediately")
		}
	})
}

func TestMetricsAbsentClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"NotFound is definitive absence", apierrors.NewNotFound(schema.GroupResource{Group: "metrics.k8s.io", Resource: "pods"}, ""), true},
		{"NoKindMatch is definitive absence", &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "metrics.k8s.io", Kind: "NodeMetrics"}}, true},
		{"internal error is transient", apierrors.NewInternalError(stderrors.New("boom")), false},
		{"plain error is transient", stderrors.New("connection refused"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := metricsAbsent(tt.err); got != tt.want {
				t.Errorf("metricsAbsent(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// --- disk capability -----------------------------------------------------------

func TestDiskCapability(t *testing.T) {
	coreObjs := []runtime.Object{
		testNode("n1", true, nil, allocList("4", "16Gi", "50Gi", "110")),
	}
	c, _ := newTestCollector(t, coreObjs, nil)

	liveDisk := model.Disk{CapacityBytes: 100, UsedBytes: 40, AvailableBytes: 60, Live: true}
	fallback := model.Disk{CapacityBytes: 50 << 30, Live: false}
	transient := apierrors.NewInternalError(stderrors.New("kubelet timeout"))
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "n1", stderrors.New("nodes/proxy denied"))

	fetchCalls := 0
	var injected error
	c.diskFetch = func(ctx context.Context, names []string) (map[string]model.Disk, error) {
		fetchCalls++
		if injected != nil {
			return map[string]model.Disk{}, injected
		}
		return map[string]model.Disk{"n1": liveDisk}, nil
	}

	// Order-dependent sequence: each step is one poll; refresh rewinds the
	// 60s gate so the poll actually refetches.
	steps := []struct {
		name      string
		refresh   bool
		err       error
		wantFetch bool
		wantCap   bool
		wantLive  bool // node carries the fetched disk vs the capacity-only fallback
	}{
		{"initial success", true, nil, true, true, true},
		{"fresh cache skips the fetch entirely", false, transient, false, true, true},
		{"forbidden drops capability immediately", true, forbidden, true, false, false},
		{"success restores after forbidden", true, nil, true, true, true},
		{"transient failure 1 keeps cache and capability", true, transient, true, true, true},
		{"transient failure 2 keeps cache and capability", true, transient, true, true, true},
		{"success mid-streak resets the counter", true, nil, true, true, true},
		{"post-reset failure 1", true, transient, true, true, true},
		{"post-reset failure 2 (would flip without the reset)", true, transient, true, true, true},
		{"post-reset failure 3 drops capability and cache", true, transient, true, false, false},
		{"failure 4 stays down", true, transient, true, false, false},
		{"success restores after the streak", true, nil, true, true, true},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			if step.refresh {
				forceDiskRefresh(c)
			}
			injected = step.err
			before := fetchCalls
			c.poll(context.Background())
			if fetched := fetchCalls > before; fetched != step.wantFetch {
				t.Errorf("fetch ran = %v, want %v", fetched, step.wantFetch)
			}
			snap := c.Latest()
			if snap.Capabilities.Disk != step.wantCap {
				t.Errorf("Capabilities.Disk = %v, want %v", snap.Capabilities.Disk, step.wantCap)
			}
			want := fallback
			if step.wantLive {
				want = liveDisk
			}
			if got := snap.Nodes[0].Disk; got != want {
				t.Errorf("node disk = %+v, want %+v", got, want)
			}
		})
	}
}

// --- publish / subscribe ---------------------------------------------------------

func TestSubscribePublish(t *testing.T) {
	c := New(&kube.Clients{Core: corefake.NewSimpleClientset(), Metrics: metricsfake.NewSimpleClientset()}, time.Second)
	c.logf = t.Logf

	ch, cancel := c.Subscribe()
	s1, s2, s3 := &model.Snapshot{}, &model.Snapshot{}, &model.Snapshot{}

	// Second publish lands on a full channel; it must not block and Latest()
	// must still advance.
	c.publish(s1)
	c.publish(s2)
	if c.Latest() != s2 {
		t.Error("Latest() did not advance while the subscriber channel was full")
	}
	select {
	case got := <-ch:
		if got != s1 {
			t.Error("buffered snapshot is not the first published one")
		}
	default:
		t.Fatal("no snapshot delivered to subscriber")
	}
	select {
	case <-ch:
		t.Fatal("second snapshot was queued; channel should hold at most one")
	default:
	}

	// A drained subscriber gets the next snapshot.
	c.publish(s3)
	select {
	case got := <-ch:
		if got != s3 {
			t.Error("drained subscriber did not receive the newest snapshot")
		}
	default:
		t.Fatal("no snapshot delivered after draining")
	}

	// After cancel, nothing more is delivered.
	cancel()
	c.publish(&model.Snapshot{})
	select {
	case <-ch:
		t.Fatal("received a snapshot after cancel")
	default:
	}
}

// --- mock source ------------------------------------------------------------------

func TestMockUsageSums(t *testing.T) {
	m := NewMock(1)
	snap := m.Latest()
	if snap == nil {
		t.Fatal("mock Latest() = nil")
	}
	if !snap.Capabilities.Metrics || !snap.Capabilities.Disk {
		t.Errorf("mock capabilities = %+v, want both true", snap.Capabilities)
	}
	if len(snap.Nodes) == 0 {
		t.Fatal("mock snapshot has no nodes")
	}
	for _, n := range snap.Nodes {
		if len(n.Pods) == 0 {
			t.Errorf("node %s has no pods", n.Name)
			continue
		}
		if !n.HasUsage {
			t.Errorf("node %s HasUsage = false, want true", n.Name)
		}
		var sum model.Resources
		for _, p := range n.Pods {
			if !p.HasUsage {
				t.Errorf("pod %s/%s HasUsage = false, want true", p.Namespace, p.Name)
			}
			sum.CPUMillis += p.Usage.CPUMillis
			sum.MemBytes += p.Usage.MemBytes
		}
		if sum != n.Usage {
			t.Errorf("node %s usage = %+v, want the sum of its pods %+v", n.Name, n.Usage, sum)
		}
	}
}
