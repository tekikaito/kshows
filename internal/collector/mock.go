package collector

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/tekikaito/kshows/internal/model"
)

// Mock is a Source that simulates a small cluster with drifting usage. It
// exists for demos and frontend development (`kshows --mock`); it never
// touches a real API server.
type Mock struct {
	mu     sync.RWMutex
	latest *model.Snapshot
	subs   map[chan *model.Snapshot]struct{}

	rng   *rand.Rand
	nodes []*mockNode
}

type mockNode struct {
	name     string
	roles    []string
	alloc    model.Allocatable
	diskUsed float64 // fraction of capacity
	pods     []*mockPod
}

type mockPod struct {
	uid, name, ns string
	req, lim      model.Resources
	// usage drifts as a random walk within [floor, ceil] * request
	cpuFrac, memFrac float64
}

func NewMock(seed int64) *Mock {
	m := &Mock{
		subs: make(map[chan *model.Snapshot]struct{}),
		rng:  rand.New(rand.NewSource(seed)),
	}
	m.build()
	m.latest = m.snapshot()
	return m
}

var mockNamespaces = []struct {
	name    string
	weight  int
	cpuBase int64 // millicores request per pod, order of magnitude
	memBase int64 // bytes request per pod
}{
	{"prod", 5, 500, 768 << 20},
	{"staging", 3, 250, 384 << 20},
	{"ingress", 1, 200, 256 << 20},
	{"monitoring", 2, 300, 1 << 30},
	{"kube-system", 3, 100, 192 << 20},
	{"batch", 2, 800, 512 << 20},
	{"data", 2, 1000, 2 << 30},
}

func (m *Mock) build() {
	type shape struct {
		cpu  int64 // millicores
		mem  int64 // bytes
		disk int64
	}
	shapes := []shape{
		{8000, 32 << 30, 200 << 30},
		{8000, 32 << 30, 200 << 30},
		{16000, 64 << 30, 400 << 30},
		{16000, 64 << 30, 400 << 30},
		{4000, 16 << 30, 100 << 30},
		{4000, 16 << 30, 100 << 30},
		{4000, 8 << 30, 100 << 30},
	}
	uid := 0
	for i, s := range shapes {
		n := &mockNode{
			name:     fmt.Sprintf("node-%02d", i+1),
			alloc:    model.Allocatable{CPUMillis: s.cpu, MemBytes: s.mem, DiskBytes: s.disk, Pods: 110},
			diskUsed: 0.2 + m.rng.Float64()*0.6,
		}
		if i == 0 {
			n.roles = []string{"control-plane"}
		}
		// Fill each node to a different level so the grid shows imbalance —
		// the "one node packed while another idles" story from the PRD.
		targetFill := []float64{0.55, 0.9, 0.45, 0.7, 0.85, 0.3, 0.6}[i]
		var usedCPU int64
		for usedCPU < int64(float64(s.cpu)*targetFill) {
			ns := mockNamespaces[m.rng.Intn(len(mockNamespaces))]
			if m.rng.Intn(10) > ns.weight+3 {
				continue
			}
			jitter := 0.5 + m.rng.Float64()*1.5
			req := model.Resources{
				CPUMillis: int64(float64(ns.cpuBase) * jitter),
				MemBytes:  int64(float64(ns.memBase) * jitter),
			}
			uid++
			p := &mockPod{
				uid:  fmt.Sprintf("mock-uid-%04d", uid),
				name: fmt.Sprintf("%s-%s-%05d", ns.name, []string{"api", "worker", "web", "cache", "agent", "job"}[m.rng.Intn(6)], uid),
				ns:   ns.name,
				req:  req,
				lim: model.Resources{
					CPUMillis: req.CPUMillis * 2,
					MemBytes:  req.MemBytes * 2,
				},
				// Most pods use well under their request (the over-provisioning
				// story); a few run hot or above request.
				cpuFrac: 0.15 + m.rng.Float64()*0.75,
				memFrac: 0.25 + m.rng.Float64()*0.85,
			}
			if m.rng.Intn(12) == 0 {
				p.cpuFrac = 1.1 + m.rng.Float64()*0.4 // bursting past request
			}
			n.pods = append(n.pods, p)
			usedCPU += req.CPUMillis
		}
		m.nodes = append(m.nodes, n)
	}
}

// Run pushes a fresh snapshot on each tick, drifting usage a little so the
// UI's transitions are visible.
func (m *Mock) Run(ctx context.Context) error {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.drift()
			snap := m.snapshot()
			m.mu.Lock()
			m.latest = snap
			for ch := range m.subs {
				select {
				case ch <- snap:
				default:
				}
			}
			m.mu.Unlock()
		}
	}
}

func (m *Mock) drift() {
	for _, n := range m.nodes {
		n.diskUsed = clamp(n.diskUsed+(m.rng.Float64()-0.495)*0.01, 0.05, 0.97)
		for _, p := range n.pods {
			p.cpuFrac = clamp(p.cpuFrac+(m.rng.Float64()-0.5)*0.12, 0.05, 1.6)
			p.memFrac = clamp(p.memFrac+(m.rng.Float64()-0.5)*0.04, 0.1, 1.3)
		}
	}
}

func (m *Mock) snapshot() *model.Snapshot {
	snap := &model.Snapshot{
		GeneratedAt:  time.Now().UTC(),
		Capabilities: model.Capabilities{Metrics: true, Disk: true},
	}
	for _, n := range m.nodes {
		mn := model.Node{
			Name:        n.name,
			Ready:       true,
			Roles:       n.roles,
			Allocatable: n.alloc,
			Disk: model.Disk{
				CapacityBytes:  n.alloc.DiskBytes,
				UsedBytes:      int64(float64(n.alloc.DiskBytes) * n.diskUsed),
				AvailableBytes: int64(float64(n.alloc.DiskBytes) * (1 - n.diskUsed)),
				Live:           true,
			},
		}
		for _, p := range n.pods {
			usage := model.Resources{
				CPUMillis: int64(float64(p.req.CPUMillis) * p.cpuFrac),
				MemBytes:  int64(float64(p.req.MemBytes) * p.memFrac),
			}
			mn.Usage.CPUMillis += usage.CPUMillis
			mn.Usage.MemBytes += usage.MemBytes
			mn.HasUsage = true
			mn.Pods = append(mn.Pods, model.Pod{
				UID: p.uid, Name: p.name, Namespace: p.ns,
				Requests: p.req, Limits: p.lim, Usage: usage, HasUsage: true,
			})
		}
		snap.Nodes = append(snap.Nodes, mn)
	}
	return snap
}

func (m *Mock) Latest() *model.Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.latest
}

func (m *Mock) Subscribe() (<-chan *model.Snapshot, func()) {
	ch := make(chan *model.Snapshot, 1)
	m.mu.Lock()
	m.subs[ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		delete(m.subs, ch)
		m.mu.Unlock()
	}
}

func clamp(v, lo, hi float64) float64 {
	return math.Min(hi, math.Max(lo, v))
}
