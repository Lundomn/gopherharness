package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/Lundomn/gopherharness/internal/evaluation"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	o := evaluation.LoadOptions{}
	flag.IntVar(&o.Requests, "requests", 200, "number of local mock requests")
	flag.IntVar(&o.Concurrency, "concurrency", 16, "request producers")
	flag.IntVar(&o.Sessions, "sessions", 16, "independent session lanes")
	flag.IntVar(&o.UserSlots, "user-slots", 4, "foreground capacity")
	flag.IntVar(&o.SystemSlots, "system-slots", 2, "background capacity")
	flag.IntVar(&o.BackgroundEvery, "background-every", 5, "make every Nth request background; 0 disables")
	flag.IntVar(&o.CancelEvery, "cancel-every", 10, "cancel every Nth request; 0 disables")
	flag.IntVar(&o.FailEvery, "fail-every", 0, "inject every Nth mock call failure; 0 disables")
	flag.DurationVar(&o.ModelLatency, "model-latency", 2*time.Millisecond, "local mock latency")
	flag.DurationVar(&o.RequestTimeout, "request-timeout", 5*time.Second, "per-request timeout, including queueing")
	flag.StringVar(&o.WorkspaceParent, "workspace-parent", "", "parent for a new temporary workspace")
	flag.BoolVar(&o.KeepWorkspace, "keep-workspace", false, "retain only this run's temporary workspace")
	artifact := flag.String("artifact", "artifacts/offline-load.json", "report path")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	r, err := evaluation.RunLoad(ctx, o)
	if writeErr := evaluation.WriteLoadReport(*artifact, r); writeErr != nil {
		fmt.Fprintln(os.Stderr, writeErr)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("offline mock only: requests=%d outcomes=%v p50=%.2fms p95=%.2fms p99=%.2fms joined=%t report=%s\n", o.Requests, r.Outcomes, r.P50MS, r.P95MS, r.P99MS, r.ResourcesJoined, *artifact)
}
