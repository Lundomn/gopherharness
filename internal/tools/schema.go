package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

func compileSchema(raw string) (*jsonschema.Resolved, error) {
	data, err := JSONSchema(raw)
	if err != nil {
		return nil, err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, err
	}
	return schema.Resolve(nil)
}

// JSONSchema expands builtin shorthand into the same schema sent to models
// and used by local validation. Callers never advertise a different contract.
func JSONSchema(raw string) (json.RawMessage, error) {
	var object map[string]any
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		return nil, err
	}
	if len(object) == 0 {
		object = map[string]any{"type": "object"}
	}
	if err := localReferencesOnly(object); err != nil {
		return nil, err
	}
	if _, standard := object["type"]; !standard {
		properties := map[string]any{}
		var required []string
		for key, value := range object {
			typeName, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("invalid schema field %q", key)
			}
			optional := strings.HasSuffix(typeName, "?")
			typeName = strings.TrimSuffix(typeName, "?")
			if typeName == "int" {
				typeName = "integer"
			}
			properties[key] = map[string]any{"type": typeName}
			if !optional {
				required = append(required, key)
			}
		}
		object = map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		sort.Strings(required)
		if len(required) > 0 {
			object["required"] = required
		}
	}
	data, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func localReferencesOnly(value any) error {
	switch v := value.(type) {
	case map[string]any:
		for k, child := range v {
			if k == "$ref" || k == "$dynamicRef" || k == "$recursiveRef" {
				if ref, ok := child.(string); !ok || !strings.HasPrefix(ref, "#") {
					return fmt.Errorf("external schema references are disabled")
				}
			}
			if err := localReferencesOnly(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := localReferencesOnly(child); err != nil {
				return err
			}
		}
	}
	return nil
}

// RegisterAll validates the entire directory before atomic publication.
func (r *Registry) RegisterAll(definitions []Definition) error {
	tmp := &Registry{specs: map[string]Spec{}, handlers: map[string]Handler{}}
	for _, d := range definitions {
		if err := tmp.Register(d); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initializeLocked()
	for name := range tmp.specs {
		if _, exists := r.specs[name]; exists {
			return fmt.Errorf("duplicate tool %q", name)
		}
	}
	for name, spec := range tmp.specs {
		r.specs[name] = spec
		r.handlers[name] = tmp.handlers[name]
		r.validators[name] = tmp.validators[name]
		r.capabilities[name] = tmp.capabilities[name]
		r.generation++
		r.generations[name] = r.generation
	}
	return nil
}
