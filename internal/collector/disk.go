package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"

	"github.com/tekikaito/kshows/internal/model"
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

const diskFetchConcurrency = 8

// fetchDisk queries /api/v1/nodes/{node}/proxy/stats/summary for every node
// with bounded concurrency. It returns per-node disk stats and whether the
// signal is available at all (false when every fetch failed, e.g. nodes/proxy
// is forbidden on this cluster).
func (c *Collector) fetchDisk(ctx context.Context, nodeNames []string) (map[string]model.Disk, bool) {
	results := make(map[string]model.Disk, len(nodeNames))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, diskFetchConcurrency)

	anyOK := false
	for _, name := range nodeNames {
		wg.Add(1)
		sem <- struct{}{}
		go func(name string) {
			defer wg.Done()
			defer func() { <-sem }()
			disk, err := c.fetchNodeDisk(ctx, name)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if errors.IsForbidden(err) {
					c.noteDiskForbidden()
				}
				return
			}
			results[name] = disk
			anyOK = true
		}(name)
	}
	wg.Wait()
	return results, anyOK
}

func (c *Collector) fetchNodeDisk(ctx context.Context, nodeName string) (model.Disk, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := c.clients.Core.CoreV1().RESTClient().Get().
		Resource("nodes").Name(nodeName).
		SubResource("proxy").Suffix("stats/summary").
		Do(ctx).Raw()
	if err != nil {
		return model.Disk{}, err
	}
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

// noteDiskForbidden logs the RBAC fallback once instead of once per node per poll.
func (c *Collector) noteDiskForbidden() {
	c.diskForbiddenOnce.Do(func() {
		c.logf("nodes/proxy is forbidden: falling back to capacity-only disk from node allocatable (grant nodes/proxy get for live disk usage)")
	})
}
