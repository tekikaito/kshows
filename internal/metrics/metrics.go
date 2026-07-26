// Package metrics exposes kshows' own operational metrics in Prometheus
// format: how the collector is doing its job, not what it found.
//
// It deliberately does not export cluster capacity as metrics. Per-node and
// per-pod series are kube-state-metrics' job; duplicating them here would
// explode label cardinality on large clusters and copy workload names into a
// second system that outlives the dashboard. Everything below is either a
// fixed-cardinality counter or a cluster-wide total.
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tekikaito/kshows/internal/model"
)

// Registry is private to kshows rather than the global default, so nothing a
// dependency registers behind our back ends up on this endpoint.
var Registry = prometheus.NewRegistry()

const namespace = "kshows"

var (
	buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "Build information. Always 1; read the version label.",
	}, []string{"version"})

	pollDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "poll_duration_seconds",
		Help:      "Time to assemble one snapshot, including the Metrics Server fetch.",
		// A poll should finish well inside the 15s interval; the buckets are
		// placed to make "approaching the interval" visible before it overruns.
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15},
	})

	pollTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "poll_total",
		Help:      "Snapshot polls by outcome.",
	}, []string{"result"})

	signalTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "signal_requests_total",
		Help: "Optional-signal fetches by outcome. " +
			"absent means the API is not installed or is forbidden (definitive); " +
			"error means a transient failure subject to hysteresis.",
	}, []string{"signal", "result"})

	streamClients = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "stream_clients",
		Help:      "Currently connected SSE clients.",
	})
)

// Signal names and outcomes, so callers cannot drift from the label values.
const (
	SignalMetrics = "metrics"
	SignalDisk    = "disk"

	ResultSuccess = "success"
	ResultAbsent  = "absent"
	ResultError   = "error"
)

func init() {
	Registry.MustRegister(
		buildInfo, pollDuration, pollTotal, signalTotal, streamClients,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// SetVersion records the running build.
func SetVersion(version string) {
	buildInfo.WithLabelValues(version).Set(1)
}

// ObservePoll records one completed poll.
func ObservePoll(d time.Duration, err error) {
	pollDuration.Observe(d.Seconds())
	result := ResultSuccess
	if err != nil {
		result = ResultError
	}
	pollTotal.WithLabelValues(result).Inc()
}

// RecordSignal records the outcome of one optional-signal fetch.
func RecordSignal(signal, result string) {
	signalTotal.WithLabelValues(signal, result).Inc()
}

// StreamConnected and StreamDisconnected track live SSE subscribers. A gap
// between them that never closes means the stream handler is leaking.
func StreamConnected()    { streamClients.Inc() }
func StreamDisconnected() { streamClients.Dec() }

// snapshotCollector reports the state of the most recent snapshot. It reads at
// scrape time rather than caching a copy on publish, so the values can never
// drift from what the API is actually serving — and it works unchanged for the
// mock source.
type snapshotCollector struct {
	latest func() *model.Snapshot

	nodes      *prometheus.Desc
	pods       *prometheus.Desc
	capability *prometheus.Desc
	timestamp  *prometheus.Desc
}

var (
	snapMu      sync.Mutex
	snapCurrent prometheus.Collector
)

// WatchSnapshots registers snapshot-derived gauges backed by latest. It takes
// a function rather than a collector.Source to keep this package free of any
// dependency on the collector, which imports it.
//
// Calling it again replaces the previous source rather than panicking on a
// duplicate registration, so constructing more than one server (as tests do)
// is safe.
func WatchSnapshots(latest func() *model.Snapshot) {
	snapMu.Lock()
	defer snapMu.Unlock()
	if snapCurrent != nil {
		Registry.Unregister(snapCurrent)
	}
	c := &snapshotCollector{
		latest: latest,
		nodes: prometheus.NewDesc(
			namespace+"_snapshot_nodes", "Nodes in the most recent snapshot.", nil, nil),
		pods: prometheus.NewDesc(
			namespace+"_snapshot_pods", "Scheduled, non-terminal pods in the most recent snapshot.", nil, nil),
		capability: prometheus.NewDesc(
			namespace+"_capability",
			"Whether an optional signal is live (1) or degraded (0). Alert on this to catch a cluster where kshows has silently lost a dimension.",
			[]string{"signal"}, nil),
		timestamp: prometheus.NewDesc(
			namespace+"_snapshot_timestamp_seconds",
			"Unix time the most recent snapshot was generated; subtract from now() for staleness.",
			nil, nil),
	}
	Registry.MustRegister(c)
	snapCurrent = c
}

func (s *snapshotCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- s.nodes
	ch <- s.pods
	ch <- s.capability
	ch <- s.timestamp
}

func (s *snapshotCollector) Collect(ch chan<- prometheus.Metric) {
	snap := s.latest()
	if snap == nil {
		// Before the first poll there is nothing to report. Emitting zeros
		// would be indistinguishable from an empty cluster.
		return
	}
	pods := 0
	for _, n := range snap.Nodes {
		pods += len(n.Pods)
	}
	ch <- prometheus.MustNewConstMetric(s.nodes, prometheus.GaugeValue, float64(len(snap.Nodes)))
	ch <- prometheus.MustNewConstMetric(s.pods, prometheus.GaugeValue, float64(pods))
	ch <- prometheus.MustNewConstMetric(s.timestamp, prometheus.GaugeValue, float64(snap.GeneratedAt.Unix()))
	ch <- prometheus.MustNewConstMetric(s.capability, prometheus.GaugeValue, boolValue(snap.Capabilities.Metrics), SignalMetrics)
	ch <- prometheus.MustNewConstMetric(s.capability, prometheus.GaugeValue, boolValue(snap.Capabilities.Disk), SignalDisk)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Handler serves the Prometheus exposition endpoint.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{Registry: Registry})
}
