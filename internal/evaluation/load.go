package evaluation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/governance"
	"github.com/Lundomn/gopherharness/internal/permission"
	"github.com/Lundomn/gopherharness/internal/provider"
	agentruntime "github.com/Lundomn/gopherharness/internal/runtime"
	"github.com/Lundomn/gopherharness/internal/scheduler"
)

type LoadOptions struct {
	Requests        int           `json:"requests"`
	Concurrency     int           `json:"concurrency"`
	Sessions        int           `json:"sessions"`
	UserSlots       int           `json:"user_slots"`
	SystemSlots     int           `json:"system_slots"`
	BackgroundEvery int           `json:"background_every"`
	CancelEvery     int           `json:"cancel_every"`
	FailEvery       int           `json:"fail_every"`
	ModelLatency    time.Duration `json:"model_latency_ns"`
	RequestTimeout  time.Duration `json:"request_timeout_ns"`
	WorkspaceParent string        `json:"workspace_parent,omitempty"`
	KeepWorkspace   bool          `json:"keep_workspace"`
}
type LoadSample struct {
	Index     int     `json:"index"`
	Outcome   string  `json:"outcome"`
	LatencyMS float64 `json:"latency_ms"`
}
type LoadReport struct {
	Schema                     string         `json:"schema"`
	Scope                      string         `json:"scope"`
	Config                     LoadOptions    `json:"config"`
	Workspace                  string         `json:"workspace"`
	ElapsedMS                  float64        `json:"elapsed_ms"`
	RequestsPerSecond          float64        `json:"requests_per_second"`
	P50MS                      float64        `json:"p50_ms"`
	P95MS                      float64        `json:"p95_ms"`
	P99MS                      float64        `json:"p99_ms"`
	Outcomes                   map[string]int `json:"outcomes"`
	PeakModelCalls             int64          `json:"peak_model_calls"`
	ActiveModelCallsAfterClose int64          `json:"active_model_calls_after_close"`
	ResourcesJoined            bool           `json:"resources_joined"`
	GoroutinesBefore           int            `json:"goroutines_before"`
	GoroutinesAfter            int            `json:"goroutines_after"`
	WorkspaceRemoved           bool           `json:"workspace_removed"`
	Samples                    []LoadSample   `json:"samples"`
}
type loadClient struct {
	latency             time.Duration
	failEvery           int64
	active, peak, calls atomic.Int64
}

func (p *loadClient) Name() string  { return "offline-load-mock" }
func (p *loadClient) Model() string { return "deterministic-mock-not-a-model-score" }
func (p *loadClient) Complete(ctx context.Context, _ string, _ int) (provider.Completion, error) {
	n := p.active.Add(1)
	defer p.active.Add(-1)
	for old := p.peak.Load(); n > old; old = p.peak.Load() {
		if p.peak.CompareAndSwap(old, n) {
			break
		}
	}
	call := p.calls.Add(1)
	t := time.NewTimer(p.latency)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return provider.Completion{}, ctx.Err()
	case <-t.C:
	}
	if p.failEvery > 0 && call%p.failEvery == 0 {
		return provider.Completion{}, errors.New("injected offline model failure")
	}
	return provider.Completion{Text: "<final>offline load result</final>", ActualModel: p.Model()}, nil
}

// RunLoad exercises real Agent/Session and Scheduler paths with a local mock
// model. The resulting throughput describes this machine/configuration only;
// it is neither model task quality nor sustained production capacity.
func RunLoad(ctx context.Context, o LoadOptions) (LoadReport, error) {
	r := LoadReport{Schema: "gopherharness.offline-load.v1", Scope: "local runtime and scheduler with deterministic mock model; not model quality or production capacity", Config: o, Outcomes: map[string]int{}, GoroutinesBefore: runtime.NumGoroutine()}
	if o.Requests < 1 || o.Requests > 100000 || o.Concurrency < 1 || o.Concurrency > 1024 || o.Sessions < 1 || o.UserSlots < 1 || o.SystemSlots < 1 || o.ModelLatency < 0 || o.RequestTimeout <= 0 || o.CancelEvery < 0 || o.FailEvery < 0 || o.BackgroundEvery < 0 {
		return r, errors.New("invalid load parameters: positive bounded requests/concurrency/sessions/slots/timeout required")
	}
	root, err := os.MkdirTemp(o.WorkspaceParent, "gopherharness-load-")
	if err != nil {
		return r, err
	}
	r.Workspace = root
	client := &loadClient{latency: o.ModelLatency, failEvery: int64(o.FailEvery)}
	cfg := config.Defaults()
	service := agentruntime.NewService(agentruntime.Options{Root: root, Config: cfg, Provider: client, Approval: permission.Never, AllowedTools: []string{"read_file"}, MaxSteps: 1, DisableAutoDream: true, FinalReadiness: governance.Off}, o.UserSlots, o.SystemSlots)
	started := time.Now()
	samples := make([]LoadSample, o.Requests)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < o.Concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				start := time.Now()
				requestCtx, cancel := context.WithTimeout(ctx, o.RequestTimeout)
				var cancelTimer *time.Timer
				if o.CancelEvery > 0 && (i+1)%o.CancelEvery == 0 {
					cancelTimer = time.AfterFunc(max(time.Microsecond, o.ModelLatency/2), cancel)
				}
				req := scheduler.Request{Conversation: fmt.Sprintf("load-%d", i%o.Sessions), Text: fmt.Sprintf("Offline request %d: return the fixed result.", i), Background: o.BackgroundEvery > 0 && (i+1)%o.BackgroundEvery == 0, Source: scheduler.Source{Channel: "load", ChatID: "local", SenderID: "fixture"}, Busy: scheduler.Append}
				handle, e := service.Scheduler.Submit(requestCtx, req)
				outcome := "completed"
				if e == nil {
					_, e = handle.Wait(requestCtx)
				} else {
					outcome = "admission_rejected"
				}
				if e != nil {
					switch {
					case errors.Is(e, context.Canceled):
						outcome = "cancelled"
					case errors.Is(e, context.DeadlineExceeded):
						outcome = "timed_out"
					case outcome != "admission_rejected":
						outcome = "execution_failed"
					}
				}
				if cancelTimer != nil {
					cancelTimer.Stop()
				}
				cancel()
				samples[i] = LoadSample{Index: i, Outcome: outcome, LatencyMS: float64(time.Since(start)) / float64(time.Millisecond)}
			}
		}()
	}
	for i := 0; i < o.Requests; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	r.ElapsedMS = float64(time.Since(started)) / float64(time.Millisecond)
	// Close is joined separately from request latency, including work whose
	// caller already timed out. Global goroutine counts remain diagnostic only.
	cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	closeErr := service.Close(cleanup)
	r.ActiveModelCallsAfterClose = client.active.Load()
	r.PeakModelCalls = client.peak.Load()
	r.ResourcesJoined = closeErr == nil && r.ActiveModelCallsAfterClose == 0
	r.GoroutinesAfter = runtime.NumGoroutine()
	r.Samples = samples
	latencies := make([]float64, len(samples))
	for i, s := range samples {
		r.Outcomes[s.Outcome]++
		latencies[i] = s.LatencyMS
	}
	sort.Float64s(latencies)
	r.P50MS = quantile(latencies, .5)
	r.P95MS = quantile(latencies, .95)
	r.P99MS = quantile(latencies, .99)
	if r.ElapsedMS > 0 {
		r.RequestsPerSecond = float64(o.Requests) / (r.ElapsedMS / 1000)
	}
	if !o.KeepWorkspace && r.ResourcesJoined {
		if err := os.RemoveAll(root); err != nil {
			return r, err
		}
		r.WorkspaceRemoved = true
	}
	return r, closeErr
}
func quantile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := max(0, min(len(sorted)-1, int(math.Ceil(float64(len(sorted))*p))-1))
	return sorted[index]
}
func WriteLoadReport(path string, r LoadReport) error { return writeJSON(path, r) }
