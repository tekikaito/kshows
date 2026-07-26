// Package model defines the snapshot types shared between the collector and
// the JSON API. The wire format is documented in the PRD (§8).
package model

import "time"

// Resources holds a CPU/memory pair. CPU is in millicores, memory in bytes.
type Resources struct {
	CPUMillis int64 `json:"cpuMillis"`
	MemBytes  int64 `json:"memBytes"`
}

// Allocatable is a node's usable capacity (Node.status.allocatable).
type Allocatable struct {
	CPUMillis int64 `json:"cpuMillis"`
	MemBytes  int64 `json:"memBytes"`
	DiskBytes int64 `json:"diskBytes"`
	Pods      int64 `json:"pods"`
}

// Disk is node-level filesystem usage from the kubelet Summary API.
// Live is false when only capacity (from allocatable) is known.
type Disk struct {
	CapacityBytes  int64 `json:"capacityBytes"`
	UsedBytes      int64 `json:"usedBytes"`
	AvailableBytes int64 `json:"availableBytes"`
	Live           bool  `json:"live"`
}

// Pod is one workload placed on a node.
type Pod struct {
	UID       string    `json:"uid"`
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	Requests  Resources `json:"requests"`
	Limits    Resources `json:"limits"`
	Usage     Resources `json:"usage"`
	HasUsage  bool      `json:"hasUsage"`
}

// Node is one machine and everything packed onto it.
type Node struct {
	Name        string      `json:"name"`
	Ready       bool        `json:"ready"`
	Roles       []string    `json:"roles,omitempty"`
	Allocatable Allocatable `json:"allocatable"`
	Usage       Resources   `json:"usage"`
	HasUsage    bool        `json:"hasUsage"`
	Disk        Disk        `json:"disk"`
	Pods        []Pod       `json:"pods"`
}

// Capabilities reports which optional signals are currently live so the UI
// can gray out dimensions instead of showing zeros that look like data.
type Capabilities struct {
	// Metrics is true when the Metrics Server (metrics.k8s.io) responds.
	Metrics bool `json:"metrics"`
	// Disk is true when at least one kubelet Summary API fetch succeeded.
	Disk bool `json:"disk"`
}

// Snapshot is the full current model served at /api/v1/snapshot and pushed
// over /api/v1/stream.
type Snapshot struct {
	GeneratedAt  time.Time    `json:"generatedAt"`
	Nodes        []Node       `json:"nodes"`
	Capabilities Capabilities `json:"capabilities"`
}
