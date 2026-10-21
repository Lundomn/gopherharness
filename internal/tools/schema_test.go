package tools

import (
	"context"
	"testing"

	"github.com/Lundomn/gopherharness/internal/model"
)

func TestSchemaRejectsMissingFieldsAndExternalReferences(t *testing.T) {
	r := NewRegistry()
	result := r.Run(context.Background(), nil, model.ToolCall{Name: "read_file", Args: map[string]any{"start": 1}})
	if result.ErrorCode != "invalid_parameters" {
		t.Fatal(result)
	}
	if _, err := compileSchema(`{"type":"object","properties":{"x":{"$ref":"https://example.invalid/schema"}}}`); err == nil {
		t.Fatal("external schema reference allowed")
	}
}
func TestRegisterAllRejectsPartialDirectory(t *testing.T) {
	r := NewRegistry()
	handler := func(context.Context, Host, map[string]any) (string, []string, error) { return "", nil, nil }
	err := r.RegisterAll([]Definition{{Spec: Spec{Name: "new", Schema: `{}`}, Handler: handler}, {Spec: Spec{Name: "read_file", Schema: `{}`}, Handler: handler}})
	if err == nil {
		t.Fatal("duplicate directory accepted")
	}
	if _, ok := r.Spec("new"); ok {
		t.Fatal("partially published directory")
	}
}
