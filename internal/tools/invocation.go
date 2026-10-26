package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/google/jsonschema-go/jsonschema"
)

var ErrUnknownTool = errors.New("unknown tool")

// Invocation binds validation, approval and dispatch to one catalog generation.
// Exported fields are owned copies for displaying/authorizing the action; Run
// uses an independent private copy and cannot execute a changed definition.
type Invocation struct {
	Call       model.ToolCall
	Spec       Spec
	registry   *Registry
	handler    Handler
	call       model.ToolCall
	generation uint64
	used       atomic.Bool
}

func (r *Registry) initializeLocked() {
	if r.specs == nil {
		r.specs = map[string]Spec{}
	}
	if r.handlers == nil {
		r.handlers = map[string]Handler{}
	}
	if r.validators == nil {
		r.validators = map[string]*jsonschema.Resolved{}
	}
	if r.capabilities == nil {
		r.capabilities = map[string]Capability{}
	}
	if r.owners == nil {
		r.owners = map[string]string{}
	}
	if r.generations == nil {
		r.generations = map[string]uint64{}
	}
}

func (r *Registry) Prepare(call model.ToolCall) (*Invocation, error) {
	return r.prepare(call, 0, false)
}
func (r *Registry) prepare(call model.ToolCall, expected uint64, check bool) (*Invocation, error) {
	r.mu.RLock()
	handler, ok := r.handlers[call.Name]
	spec, validator, generation := r.specs[call.Name], r.validators[call.Name], r.generations[call.Name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownTool, call.Name)
	}
	if check && generation != expected {
		return nil, errors.New("tool definition changed since model request; rebuild context and authorize again")
	}
	validated, err := validateCall(call, validator)
	if err != nil {
		return nil, err
	}
	display, err := validateCall(validated, nil)
	if err != nil {
		return nil, err
	}
	return &Invocation{Call: display, Spec: spec, registry: r, handler: handler, call: validated, generation: generation}, nil
}

func (i *Invocation) Run(ctx context.Context, h Host) model.ToolResult {
	r := model.ToolResult{Name: i.call.Name, Status: "error", Metadata: map[string]any{}}
	if err := ctx.Err(); err != nil {
		r.Error = err.Error()
		r.ErrorCode = "cancelled"
		return r
	}
	i.registry.mu.RLock()
	current, exists := i.registry.generations[i.call.Name]
	if !exists || current != i.generation {
		i.registry.mu.RUnlock()
		r.Error = "tool definition changed or was revoked; refresh and authorize again"
		r.ErrorCode = "definition_changed"
		return r
	}
	if !i.used.CompareAndSwap(false, true) {
		i.registry.mu.RUnlock()
		r.Error = "prepared tool invocation already consumed"
		r.ErrorCode = "already_invoked"
		return r
	}
	// This lock release is the admission boundary. An admitted old handler is
	// allowed to finish; a concurrent replacement only blocks later admission.
	i.registry.mu.RUnlock()
	if i.call.Name == "run_shell" && h != nil {
		h = shellRecordingHost{Host: h, metadata: r.Metadata}
	}
	out, paths, err := i.handler(ctx, h, i.call.Args)
	if err != nil {
		r.Error = err.Error()
		if strings.Contains(r.Error, "escapes workspace") {
			r.ErrorCode = "path_escape"
		}
		return r
	}
	r.Status = "ok"
	r.Output = out
	r.AffectedPaths = paths
	return r
}

// Catalog freezes the tool names/specs that were advertised to one model
// request. It prevents a response produced against an old schema from being
// interpreted using a replacement schema, even before approval starts.
type Catalog struct {
	registry    *Registry
	specs       map[string]Spec
	generations map[string]uint64
}

func (r *Registry) Snapshot() *Catalog {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c := &Catalog{registry: r, specs: map[string]Spec{}, generations: map[string]uint64{}}
	for name, spec := range r.specs {
		c.specs[name] = spec
		c.generations[name] = r.generations[name]
	}
	return c
}
func (c *Catalog) Specs() []Spec {
	specs := make([]Spec, 0, len(c.specs))
	for _, spec := range c.specs {
		specs = append(specs, spec)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs
}
func (c *Catalog) Prepare(call model.ToolCall) (*Invocation, error) {
	generation, exists := c.generations[call.Name]
	if !exists {
		return nil, fmt.Errorf("%w %q was not advertised in this request", ErrUnknownTool, call.Name)
	}
	return c.registry.prepare(call, generation, true)
}

type catalogContextKey struct{}

func WithCatalog(ctx context.Context, c *Catalog) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, catalogContextKey{}, c)
}
func CatalogFromContext(ctx context.Context) *Catalog {
	c, _ := ctx.Value(catalogContextKey{}).(*Catalog)
	return c
}
func (r *Registry) PrepareContext(ctx context.Context, call model.ToolCall) (*Invocation, error) {
	if c, ok := ctx.Value(catalogContextKey{}).(*Catalog); ok && c != nil {
		if c.registry != r {
			return nil, errors.New("tool catalog belongs to another agent registry")
		}
		return c.Prepare(call)
	}
	return r.Prepare(call)
}

// ReplaceNamespaces is a trusted owner API, never a tool available to a model.
// It validates every definition first, then atomically swaps entire service
// snapshots. Existing builtin/other-owner entries cannot be overwritten.
func (r *Registry) ReplaceNamespaces(updates map[string][]Definition) error {
	tmp := &Registry{}
	owners := map[string]string{}
	for owner, defs := range updates {
		if owner == "" {
			return errors.New("namespace owner required")
		}
		for _, d := range defs {
			if !strings.HasPrefix(d.Spec.Name, owner+"_") {
				return fmt.Errorf("tool %q outside namespace %q", d.Spec.Name, owner)
			}
			if err := tmp.Register(d); err != nil {
				return err
			}
			owners[d.Spec.Name] = owner
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initializeLocked()
	for name := range tmp.specs {
		if _, exists := r.specs[name]; exists && r.owners[name] != owners[name] {
			return fmt.Errorf("tool %q belongs to another owner", name)
		}
	}
	for name, owner := range r.owners {
		if _, replace := updates[owner]; replace {
			delete(r.specs, name)
			delete(r.handlers, name)
			delete(r.validators, name)
			delete(r.capabilities, name)
			delete(r.generations, name)
			delete(r.owners, name)
		}
	}
	for name, spec := range tmp.specs {
		r.specs[name] = spec
		r.handlers[name] = tmp.handlers[name]
		r.validators[name] = tmp.validators[name]
		r.capabilities[name] = Capability{Effect: Unknown}
		r.owners[name] = owners[name]
		r.generation++
		r.generations[name] = r.generation
	}
	return nil
}
func (r *Registry) ReplaceNamespace(owner string, definitions []Definition) error {
	return r.ReplaceNamespaces(map[string][]Definition{owner: definitions})
}
