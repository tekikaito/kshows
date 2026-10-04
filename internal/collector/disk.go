package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/tekikaito/kshows/internal/model"
)

// Node disk sources, for Options.NodeDisk and --node-disk. Both routes read
// the same kubelet Summary API; they differ in the permission they need.
const (
	// NodeDiskAuto tries the kubelet directly, then the API server proxy, and
	// sticks with the first that answers.
	NodeDiskAuto = "auto"
	// NodeDiskKubelet calls each kubelet directly. The kubelet authorizes
	// /stats/* as nodes/stats, which reads statistics and nothing else.
	NodeDiskKubelet = "kubelet"
	// NodeDiskProxy goes through the API server, which authorizes the whole
	// path as nodes/proxy. That grant also reaches the kubelet's exec and run
	// endpoints, which is why the direct route is preferred.
	NodeDiskProxy = "proxy"
	// NodeDiskOff never asks for node disk.
	NodeDiskOff = "off"
)

// statsSummary is the minimal slice of the kubelet Summary API response we
// need: the node-level filesystem stats.
type statsSummary struct {
	Node struct {
		Fs *struct {
			CapacityBytes  *uint64 `json:"capacityBytes"`
			UsedBytes      *uint64 `json:"usedBytes"`
			AvailableBytes *uint64 `json:"availableBytes"`
		} `json:"fs"`
	} `json:"node"`
}

const (
	diskFetchConcurrency = 8
	// maxSummaryBytes bounds what one kubelet answer may cost. A busy node's
	// summary is a few hundred KiB.
	maxSummaryBytes = 16 << 20
	// defaultKubeletPort is used when a Node does not report its endpoint.
	defaultKubeletPort = 10250
)

// diskRouteFunc reads one node's disk stats.
type diskRouteFunc func(ctx context.Context, node *corev1.Node) (model.Disk, error)

// fetchDisk reads node disk over the configured route. In auto mode it keeps
// using the route that last worked and probes again, kubelet first, when there
// is none. Only called from the poll goroutine.
func (c *Collector) fetchDisk(ctx context.Context, nodes []*corev1.Node) (map[string]model.Disk, error) {
	switch c.diskMode {
	case NodeDiskKubelet:
		return c.fanOut(ctx, nodes, c.kubeletDisk)
	case NodeDiskProxy:
		return c.fanOut(ctx, nodes, c.proxyDisk)
	}

	switch c.diskRoute {
	case NodeDiskKubelet, NodeDiskProxy:
		route := c.kubeletDisk
		if c.diskRoute == NodeDiskProxy {
			route = c.proxyDisk
		}
		disk, err := c.fanOut(ctx, nodes, route)
		if errors.IsForbidden(err) {
			// The permission behind this route was revoked. Probe both
			// again on the next attempt instead of insisting on this one.
			c.diskRoute = ""
		}
		return disk, err
	}

	disk, kubeletErr := c.fanOut(ctx, nodes, c.kubeletDisk)
	if kubeletErr == nil {
		c.diskRoute = NodeDiskKubelet
		c.logf("node disk: reading each kubelet directly (nodes/stats)")
		return disk, nil
	}
	disk, proxyErr := c.fanOut(ctx, nodes, c.proxyDisk)
	if proxyErr == nil {
		c.diskRoute = NodeDiskProxy
		c.logf("node disk: kubelet not reachable directly (%v); using the API server proxy (nodes/proxy)", kubeletErr)
		return disk, nil
	}
	// Neither route works. If a permission is part of the reason, report a
	// 403 so the signal is parked for forbiddenRetry rather than probed with
	// a denied request every disk interval.
	if errors.IsForbidden(proxyErr) {
		return disk, fmt.Errorf("kubelet: %v; proxy: %w", kubeletErr, proxyErr)
	}
	if errors.IsForbidden(kubeletErr) {
		return disk, fmt.Errorf("proxy: %v; kubelet: %w", proxyErr, kubeletErr)
	}
	return disk, fmt.Errorf("kubelet: %v; proxy: %w", kubeletErr, proxyErr)
}

// fanOut runs route for every node with bounded concurrency. The error is nil
// when at least one node answered, otherwise a representative failure for the
// caller to classify. A Forbidden error wins the pick because it is the
// definitive (RBAC) case, not a transient one.
func (c *Collector) fanOut(ctx context.Context, nodes []*corev1.Node, route diskRouteFunc) (map[string]model.Disk, error) {
	results := make(map[string]model.Disk, len(nodes))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, diskFetchConcurrency)

	anyOK := false
	var lastErr error
	for _, node := range nodes {
		wg.Add(1)
		sem <- struct{}{}
		go func(node *corev1.Node) {
			defer wg.Done()
			defer func() { <-sem }()
			disk, err := route(ctx, node)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if lastErr == nil || errors.IsForbidden(err) {
					lastErr = err
				}
				return
			}
			results[node.Name] = disk
			anyOK = true
		}(node)
	}
	wg.Wait()
	if anyOK {
		return results, nil
	}
	return results, lastErr
}

// fetchProxyDisk queries /api/v1/nodes/{node}/proxy/stats/summary.
func (c *Collector) fetchProxyDisk(ctx context.Context, node *corev1.Node) (model.Disk, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := c.clients.Core.CoreV1().RESTClient().Get().
		Resource("nodes").Name(node.Name).
		SubResource("proxy").Suffix("stats/summary").
		Do(ctx).Raw()
	if err != nil {
		return model.Disk{}, err
	}
	return parseSummary(raw, node.Name)
}

// fetchKubeletDisk queries https://{node address}:{kubelet port}/stats/summary
// with kshows' own credentials. The kubelet maps a 401 or 403 to a Forbidden
// error so it is classified like the proxy route's.
func (c *Collector) fetchKubeletDisk(ctx context.Context, node *corev1.Node) (model.Disk, error) {
	if c.kubeletHTTP == nil {
		return model.Disk{}, fmt.Errorf("no client for direct kubelet access: %w", c.kubeletHTTPErr)
	}
	addr := kubeletAddress(node)
	if addr == "" {
		return model.Disk{}, fmt.Errorf("node %s reports no address", node.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := "https://" + net.JoinHostPort(addr, strconv.Itoa(kubeletPort(node))) + "/stats/summary"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return model.Disk{}, err
	}
	resp, err := c.kubeletHTTP.Do(req)
	if err != nil {
		return model.Disk{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSummaryBytes))
	if err != nil {
		return model.Disk{}, fmt.Errorf("reading stats/summary from %s: %w", node.Name, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return parseSummary(raw, node.Name)
	case http.StatusUnauthorized, http.StatusForbidden:
		return model.Disk{}, errors.NewForbidden(schema.GroupResource{Resource: "nodes/stats"}, node.Name,
			fmt.Errorf("kubelet answered %d", resp.StatusCode))
	default:
		return model.Disk{}, fmt.Errorf("kubelet on %s answered %d", node.Name, resp.StatusCode)
	}
}

func parseSummary(raw []byte, nodeName string) (model.Disk, error) {
	var summary statsSummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return model.Disk{}, fmt.Errorf("parsing stats/summary for %s: %w", nodeName, err)
	}
	fs := summary.Node.Fs
	if fs == nil || fs.CapacityBytes == nil {
		return model.Disk{}, fmt.Errorf("stats/summary for %s has no node fs stats", nodeName)
	}
	disk := model.Disk{CapacityBytes: int64(*fs.CapacityBytes), Live: true}
	if fs.UsedBytes != nil {
		disk.UsedBytes = int64(*fs.UsedBytes)
	}
	if fs.AvailableBytes != nil {
		disk.AvailableBytes = int64(*fs.AvailableBytes)
	}
	return disk, nil
}

// kubeletAddress picks the address the API server itself would use for the
// kubelet: InternalIP, then ExternalIP, then Hostname.
func kubeletAddress(node *corev1.Node) string {
	for _, typ := range []corev1.NodeAddressType{corev1.NodeInternalIP, corev1.NodeExternalIP, corev1.NodeHostName} {
		for _, a := range node.Status.Addresses {
			if a.Type == typ && a.Address != "" {
				return a.Address
			}
		}
	}
	return ""
}

func kubeletPort(node *corev1.Node) int {
	if p := node.Status.DaemonEndpoints.KubeletEndpoint.Port; p > 0 {
		return int(p)
	}
	return defaultKubeletPort
}

// newKubeletClient builds an HTTP client for the kubelets from kshows' own
// API credentials. The cluster CA verifies the kubelet's serving certificate
// on distributions that sign it with that CA (k3s, and kubeadm with
// serverTLSBootstrap); elsewhere it is self-signed and only insecureTLS gets
// through, the same trade-off metrics-server makes.
func newKubeletClient(cfg *rest.Config, insecureTLS bool) (*http.Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("no API client configuration")
	}
	cfg = rest.CopyConfig(cfg)
	// A ServerName pinned for the API server would fail every kubelet check.
	cfg.ServerName = ""
	if insecureTLS {
		cfg.Insecure = true
		cfg.CAFile = ""
		cfg.CAData = nil
	}
	rt, err := rest.TransportFor(cfg)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: rt, Timeout: 10 * time.Second}, nil
}
