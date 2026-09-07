package tools

import (
	"context"
	"testing"

	"github.com/Lundomn/gopherharness/internal/model"
)

func TestBuiltinRegistryIsComplete(t *testing.T) {
	registry := NewRegistry()
	if got := len(registry.Specs()); got != 16 {
		t.Fatalf("got %d builtin tools", got)
	}
	for _, spec := range registry.Specs() {
		if registry.handlers[spec.Name] == nil {
			t.Fatalf("tool %q has no handler", spec.Name)
		}
	}
}

func TestRegistryDispatchAndRegistrationValidation(t *testing.T) {
	registry := &Registry{specs: map[string]Spec{}, handlers: map[string]Handler{}}
	definition := Definition{Spec: Spec{Name: "custom", Description: "test", Schema: `{}`}, Handler: func(_ context.Context, _ Host, args map[string]any) (string, []string, error) {
		return str(args, "value", "missing"), nil, nil
	}}
	if err := registry.Register(definition); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(definition); err == nil {
		t.Fatal("duplicate registration was accepted")
	}
	result := registry.Run(context.Background(), nil, model.ToolCall{Name: "custom", Args: map[string]any{"value": "ok"}})
	if result.Status != "ok" || result.Output != "ok" {
		t.Fatalf("result=%#v", result)
	}
	unknown := registry.Run(context.Background(), nil, model.ToolCall{Name: "missing"})
	if unknown.ErrorCode != "unknown_tool" {
		t.Fatalf("unknown=%#v", unknown)
	}
}
