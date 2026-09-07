package redact

import "testing"

func TestRecursiveRedaction(t *testing.T) {
	r := New("known-provider-secret")
	input := map[string]any{
		"authorization": "Bearer abcdefghijklmnop",
		"nested": map[string]any{
			"message":  "key=known-provider-secret token sk-abcdefghijklmnop",
			"password": "hunter2",
		},
	}
	out := r.Map(input)
	if out["authorization"] != Replacement {
		t.Fatalf("authorization not redacted: %#v", out)
	}
	nested := out["nested"].(map[string]any)
	if nested["password"] != Replacement || nested["message"] != "key=[REDACTED] token [REDACTED]" {
		t.Fatalf("nested values not redacted: %#v", nested)
	}
	if input["authorization"] == Replacement {
		t.Fatal("redaction mutated the input")
	}
}
