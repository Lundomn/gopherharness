package evaluation

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestOfflineLoadUsesBoundedRuntimeAndJoinsCancelledWork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	o := LoadOptions{Requests: 24, Concurrency: 8, Sessions: 4, UserSlots: 2, SystemSlots: 1, BackgroundEvery: 4, CancelEvery: 5, ModelLatency: 10 * time.Millisecond, RequestTimeout: 5 * time.Second, WorkspaceParent: t.TempDir()}
	r, err := RunLoad(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Samples) != 24 || !r.ResourcesJoined || !r.WorkspaceRemoved || r.PeakModelCalls > 3 || r.ActiveModelCallsAfterClose != 0 {
		t.Fatal(r)
	}
	if r.Outcomes["cancelled"] < 1 || r.Outcomes["completed"] < 1 {
		t.Fatal(r.Outcomes)
	}
	if _, err := os.Stat(r.Workspace); !os.IsNotExist(err) {
		t.Fatal("temporary load workspace not cleaned")
	}
	if r.P99MS < r.P95MS || r.P95MS < r.P50MS || r.ElapsedMS <= 0 {
		t.Fatal("invalid latency distribution", r)
	}
}

func TestOfflineLoadRejectsInvalidCapacityAndClassifiesFailures(t *testing.T) {
	if _, err := RunLoad(context.Background(), LoadOptions{}); err == nil {
		t.Fatal("accepted empty limits")
	}
	r, err := RunLoad(context.Background(), LoadOptions{Requests: 2, Concurrency: 1, Sessions: 1, UserSlots: 1, SystemSlots: 1, FailEvery: 1, RequestTimeout: time.Second, WorkspaceParent: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcomes["execution_failed"] != 2 || !r.ResourcesJoined {
		t.Fatal(r.Outcomes, r.ResourcesJoined)
	}
}

func TestNearestRankPercentile(t *testing.T) {
	if quantile([]float64{1, 100}, .95) != 100 {
		t.Fatal("p95 underreported")
	}
}
