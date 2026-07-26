package metrics

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/tekikaito/kshows/internal/model"
)

func testSnapshot() *model.Snapshot {
	return &model.Snapshot{
		GeneratedAt: time.Unix(1700000000, 0).UTC(),
		Nodes: []model.Node{
			{Name: "node-a", Pods: []model.Pod{{UID: "1"}, {UID: "2"}}},
			{Name: "node-b", Pods: []model.Pod{{UID: "3"}}},
		},
		Capabilities: model.Capabilities{Metrics: true, Disk: false},
	}
}

func TestSnapshotCollector(t *testing.T) {
	snap := testSnapshot()
	WatchSnapshots(func() *model.Snapshot { return snap })

	tests := []struct {
		name     string
		metric   string
		expected string
	}{
		{
			name:   "node count",
			metric: "kshows_snapshot_nodes",
			expected: `# HELP kshows_snapshot_nodes Nodes in the most recent snapshot.
# TYPE kshows_snapshot_nodes gauge
kshows_snapshot_nodes 2
`,
		},
		{
			name:   "pods summed across nodes",
			metric: "kshows_snapshot_pods",
			expected: `# HELP kshows_snapshot_pods Scheduled, non-terminal pods in the most recent snapshot.
# TYPE kshows_snapshot_pods gauge
kshows_snapshot_pods 3
`,
		},
		{
			name:   "timestamp for staleness",
			metric: "kshows_snapshot_timestamp_seconds",
			expected: `# HELP kshows_snapshot_timestamp_seconds Unix time the most recent snapshot was generated; subtract from now() for staleness.
# TYPE kshows_snapshot_timestamp_seconds gauge
kshows_snapshot_timestamp_seconds 1.7e+09
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := testutil.GatherAndCompare(Registry, strings.NewReader(tt.expected), tt.metric); err != nil {
				t.Error(err)
			}
		})
	}

	t.Run("capability mirrors each signal", func(t *testing.T) {
		expected := `# HELP kshows_capability Whether an optional signal is live (1) or degraded (0). Alert on this to catch a cluster where kshows has silently lost a dimension.
# TYPE kshows_capability gauge
kshows_capability{signal="disk"} 0
kshows_capability{signal="metrics"} 1
`
		if err := testutil.GatherAndCompare(Registry, strings.NewReader(expected), "kshows_capability"); err != nil {
			t.Error(err)
		}
	})
}

// Before the first poll there is no snapshot. Emitting zeros then would be
// indistinguishable from a genuinely empty cluster, so emit nothing.
func TestSnapshotCollectorSilentBeforeFirstPoll(t *testing.T) {
	WatchSnapshots(func() *model.Snapshot { return nil })

	names := []string{
		"kshows_snapshot_nodes",
		"kshows_snapshot_pods",
		"kshows_snapshot_timestamp_seconds",
		"kshows_capability",
	}
	got, err := testutil.GatherAndCount(Registry, names...)
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	if got != 0 {
		t.Errorf("got %d snapshot metrics with no snapshot, want 0", got)
	}
}

// Constructing more than one server must not panic on duplicate registration.
func TestWatchSnapshotsReplaces(t *testing.T) {
	WatchSnapshots(func() *model.Snapshot { return nil })
	WatchSnapshots(func() *model.Snapshot { return testSnapshot() })

	expected := `# HELP kshows_snapshot_nodes Nodes in the most recent snapshot.
# TYPE kshows_snapshot_nodes gauge
kshows_snapshot_nodes 2
`
	if err := testutil.GatherAndCompare(Registry, strings.NewReader(expected), "kshows_snapshot_nodes"); err != nil {
		t.Errorf("second registration did not take effect: %v", err)
	}
}

func TestObservePoll(t *testing.T) {
	okBefore := testutil.ToFloat64(pollTotal.WithLabelValues(ResultSuccess))
	errBefore := testutil.ToFloat64(pollTotal.WithLabelValues(ResultError))

	ObservePoll(10*time.Millisecond, nil)
	ObservePoll(20*time.Millisecond, errors.New("listing nodes from cache: boom"))

	if got := testutil.ToFloat64(pollTotal.WithLabelValues(ResultSuccess)); got != okBefore+1 {
		t.Errorf("success polls = %v, want %v", got, okBefore+1)
	}
	if got := testutil.ToFloat64(pollTotal.WithLabelValues(ResultError)); got != errBefore+1 {
		t.Errorf("failed polls = %v, want %v", got, errBefore+1)
	}
}

func TestRecordSignal(t *testing.T) {
	before := testutil.ToFloat64(signalTotal.WithLabelValues(SignalDisk, ResultAbsent))
	RecordSignal(SignalDisk, ResultAbsent)
	if got := testutil.ToFloat64(signalTotal.WithLabelValues(SignalDisk, ResultAbsent)); got != before+1 {
		t.Errorf("disk absent count = %v, want %v", got, before+1)
	}
}

// A gauge that never returns to its baseline means the SSE handler is leaking
// subscribers, which is the whole reason to track it.
func TestStreamClientsBalances(t *testing.T) {
	before := testutil.ToFloat64(streamClients)
	StreamConnected()
	StreamConnected()
	if got := testutil.ToFloat64(streamClients); got != before+2 {
		t.Fatalf("clients after 2 connects = %v, want %v", got, before+2)
	}
	StreamDisconnected()
	StreamDisconnected()
	if got := testutil.ToFloat64(streamClients); got != before {
		t.Errorf("clients after matching disconnects = %v, want %v", got, before)
	}
}

func TestHandlerServesExposition(t *testing.T) {
	SetVersion("v9.9.9-test")
	WatchSnapshots(func() *model.Snapshot { return testSnapshot() })

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`kshows_build_info{version="v9.9.9-test"} 1`,
		"kshows_poll_duration_seconds_bucket",
		"kshows_snapshot_nodes 2",
		`kshows_capability{signal="metrics"} 1`,
		"kshows_stream_clients",
		"go_goroutines", // the runtime collector is what catches leaks
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q", want)
		}
	}
}

// Cluster capacity belongs in kube-state-metrics, not here: per-node and
// per-pod series would explode cardinality and copy workload names into a
// second system. Nothing on this endpoint may carry a node or pod name.
func TestNoWorkloadNamesLeakIntoLabels(t *testing.T) {
	WatchSnapshots(func() *model.Snapshot { return testSnapshot() })

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	for _, name := range []string{"node-a", "node-b"} {
		if strings.Contains(rec.Body.String(), name) {
			t.Errorf("exposition leaks the workload identifier %q", name)
		}
	}
}
