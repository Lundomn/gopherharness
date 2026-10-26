package tools

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/Lundomn/gopherharness/internal/model"
)

func TestNamespaceRefreshFreezesInFlightAndRejectsStaleApproval(t *testing.T) {
	r := NewRegistry()
	started, release := make(chan struct{}), make(chan struct{})
	old := Definition{Spec: Spec{Name: "mcp_x_read", Risky: true, Schema: `{}`}, Handler: func(context.Context, Host, map[string]any) (string, []string, error) {
		close(started)
		<-release
		return "old", nil, nil
	}}
	if err := r.ReplaceNamespace("mcp_x", []Definition{old}); err != nil {
		t.Fatal(err)
	}
	catalog := r.Snapshot()
	pending, err := catalog.Prepare(model.ToolCall{Name: old.Spec.Name})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan model.ToolResult, 1)
	go func() { result <- r.Run(context.Background(), nil, model.ToolCall{Name: old.Spec.Name}) }()
	<-started
	updated := Definition{Spec: Spec{Name: old.Spec.Name, Risky: true, Schema: `{"value":"string"}`}, Handler: func(_ context.Context, _ Host, args map[string]any) (string, []string, error) {
		return args["value"].(string), nil, nil
	}}
	if err := r.ReplaceNamespace("mcp_x", []Definition{updated}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if got := <-result; got.Output != "old" || got.Status != "ok" {
		t.Fatal(got)
	}
	if got := pending.Run(context.Background(), nil); got.ErrorCode != "definition_changed" {
		t.Fatal(got)
	}
	if _, err := catalog.Prepare(model.ToolCall{Name: old.Spec.Name}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatal(err)
	}
	inv, err := r.Prepare(model.ToolCall{Name: old.Spec.Name, Args: map[string]any{"value": "frozen"}})
	if err != nil {
		t.Fatal(err)
	}
	inv.Call.Args["value"] = "mutated"
	if got := inv.Run(context.Background(), nil); got.Output != "frozen" {
		t.Fatal(got)
	}
	if got := inv.Run(context.Background(), nil); got.ErrorCode != "already_invoked" {
		t.Fatal(got)
	}
}

func TestNamespaceReplacementAtomicAndConcurrent(t *testing.T) {
	r := NewRegistry()
	def := func(name string) Definition {
		return Definition{Spec: Spec{Name: name, Risky: true, Schema: `{}`}, Handler: func(context.Context, Host, map[string]any) (string, []string, error) { return "ok", nil, nil }}
	}
	if err := r.ReplaceNamespace("mcp_x", []Definition{def("mcp_x_one")}); err != nil {
		t.Fatal(err)
	}
	invalid := def("mcp_x_bad")
	invalid.Spec.Schema = `{"type":"object","$ref":"https://untrusted/schema"}`
	if err := r.ReplaceNamespace("mcp_x", []Definition{def("mcp_x_two"), invalid}); err == nil {
		t.Fatal("invalid schema accepted")
	}
	if _, ok := r.Spec("mcp_x_one"); !ok {
		t.Fatal("invalid replacement destroyed old snapshot")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = r.Specs()
				_ = r.Snapshot().Specs()
				_ = r.Run(context.Background(), nil, model.ToolCall{Name: "mcp_x_one"})
				_ = r.Capability("mcp_x_one")
			}
		}()
	}
	for i := 0; i < 100; i++ {
		if err := r.ReplaceNamespace("mcp_x", []Definition{def("mcp_x_one")}); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if err := r.ReplaceNamespace("mcp_x", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Spec("mcp_x_one"); ok {
		t.Fatal("revoked tool retained")
	}
	if _, ok := r.Spec("read_file"); !ok {
		t.Fatal("builtin was removed")
	}
}
