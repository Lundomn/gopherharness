package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"encoding/json"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/workspace"
	"github.com/google/jsonschema-go/jsonschema"
)

type Host interface {
	Resolve(string) (string, error)
	Relative(string) string
	MarkRead(string)
	FreshRead(string) bool
	Invalidate(string)
	RootDir() string
	ShellEnvironment() []string
	ExecuteShell(context.Context, string, int) (string, error)
	InspectImage(context.Context, string, string) (string, error)
	TodoAdd(string) string
	TodoUpdate(string, string) error
	TodoList() string
	EnterPlan(string) (string, error)
	ExitPlan() string
	StartWorker(string, string) (string, error)
	SendWorker(string, string) error
	StopWorker(string) error
	AskUser(string) string
}

type Spec struct {
	Name        string
	Risky       bool
	Description string
	Schema      string
}

type Handler func(context.Context, Host, map[string]any) (string, []string, error)

type Definition struct {
	Spec    Spec
	Handler Handler
}

type Registry struct {
	mu           sync.RWMutex
	specs        map[string]Spec
	handlers     map[string]Handler
	capabilities map[string]Capability
	validators   map[string]*jsonschema.Resolved
	owners       map[string]string
	generations  map[string]uint64
	generation   uint64
}

func NewRegistry() *Registry {
	r := &Registry{specs: map[string]Spec{}, handlers: map[string]Handler{}}
	for _, definition := range builtinDefinitions() {
		_ = r.Register(definition)
		switch definition.Spec.Name {
		case "read_file", "list_files", "search", "skill_read":
			r.capabilities[definition.Spec.Name] = Capability{Effect: Read, ConcurrencySafe: true}
		case "write_file", "patch_file":
			r.capabilities[definition.Spec.Name] = Capability{Effect: Write}
		case "run_shell":
			r.capabilities[definition.Spec.Name] = Capability{Effect: Execute}
		}
	}
	return r
}

func (r *Registry) Register(definition Definition) error {
	name := strings.TrimSpace(definition.Spec.Name)
	if name == "" || definition.Handler == nil {
		return errors.New("tool definition requires a name and handler")
	}
	definition.Spec.Name = name
	validator, err := compileSchema(definition.Spec.Schema)
	if err != nil {
		return fmt.Errorf("tool %s schema: %w", name, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initializeLocked()
	if _, exists := r.specs[name]; exists {
		return fmt.Errorf("tool %q is already registered", name)
	}
	r.specs[name] = definition.Spec
	r.handlers[name] = definition.Handler
	if r.capabilities == nil {
		r.capabilities = map[string]Capability{}
	}
	r.capabilities[name] = Capability{Effect: Unknown}
	r.validators[name] = validator
	r.generation++
	r.generations[name] = r.generation
	return nil
}

func builtinDefinitions() []Definition {
	return []Definition{
		{Spec{"skill_read", false, "Read the cached body of a registered skill by name; does not execute it.", `{"name":"string"}`}, func(ctx context.Context, h Host, args map[string]any) (string, []string, error) {
			if err := ctx.Err(); err != nil {
				return "", nil, err
			}
			reader, ok := h.(interface{ ReadSkill(string) (string, error) })
			if !ok {
				return "", nil, fmt.Errorf("host does not support registered skill reads")
			}
			text, err := reader.ReadSkill(str(args, "name", ""))
			return text, nil, err
		}},
		{Spec{"list_files", false, "List up to 200 workspace entries without following symlinks.", `{"path":"string?"}`}, func(ctx context.Context, h Host, args map[string]any) (string, []string, error) {
			reader, ok := h.(interface {
				ListFiles(context.Context, string) (string, error)
			})
			if !ok {
				return "", nil, errors.New("host does not support confined directory listing")
			}
			out, err := reader.ListFiles(ctx, str(args, "path", "."))
			return out, nil, err
		}},
		{Spec{"read_file", false, "Read a UTF-8 file by line range.", `{"path":"string","start":"int?","end":"int?"}`}, func(ctx context.Context, h Host, args map[string]any) (string, []string, error) {
			if host, ok := h.(interface {
				ReadSnapshot(context.Context, string) ([]byte, error)
			}); ok {
				path := str(args, "path", "")
				start, end := integer(args, "start", 1), integer(args, "end", 200)
				if path == "" || start < 1 || end < start {
					return "", nil, errors.New("invalid path or line range")
				}
				data, e := host.ReadSnapshot(ctx, path)
				if e != nil {
					return "", nil, e
				}
				s := bufio.NewScanner(bytes.NewReader(data))
				s.Buffer(make([]byte, 4096), 64<<20)
				var lines []string
				line := 0
				outputBytes := 0
				for s.Scan() {
					if err := ctx.Err(); err != nil {
						return "", nil, err
					}
					line++
					if line >= start && line <= end {
						text := fmt.Sprintf("%4d: %s", line, s.Text())
						if outputBytes+len(text) > workspace.MaxReadToolOutputBytes {
							lines = append(lines, "[truncated: output exceeds 64 KiB; narrow the line range]")
							break
						}
						lines = append(lines, text)
						outputBytes += len(text) + 1
					}
					if line >= end {
						break
					}
				}
				return "# " + path + "\n" + strings.Join(lines, "\n"), nil, s.Err()
			}
			return "", nil, errors.New("host does not support confined snapshot reads")
		}},
		{Spec{"search", false, "Search confined workspace text; at most 200 matches, 4 MiB/file and 32 MiB scanned per call.", `{"pattern":"string","path":"string?"}`}, func(ctx context.Context, h Host, args map[string]any) (string, []string, error) {
			reader, ok := h.(interface {
				SearchFiles(context.Context, string, string) (string, error)
			})
			if !ok {
				return "", nil, errors.New("host does not support confined file search")
			}
			out, err := reader.SearchFiles(ctx, str(args, "pattern", ""), str(args, "path", "."))
			return out, nil, err
		}},
		{Spec{"inspect_image", false, "Understand an image with the configured vision model.", `{"path":"string","question":"string?","profile":"string?","output_schema":"string?"}`}, inspectImage},
		{Spec{"run_shell", true, "Run a shell command in the workspace.", `{"command":"string","timeout":"int?"}`}, func(ctx context.Context, h Host, args map[string]any) (string, []string, error) {
			return runShell(ctx, h, str(args, "command", ""), integer(args, "timeout", 60))
		}},
		{Spec{"write_file", true, "Write a text file.", `{"path":"string","content":"string"}`}, func(ctx context.Context, h Host, args map[string]any) (string, []string, error) {
			if host, ok := h.(interface {
				WriteFile(context.Context, string, []byte) error
			}); ok {
				p := str(args, "path", "")
				if p == "" {
					return "", nil, errors.New("path required")
				}
				e := host.WriteFile(ctx, p, []byte(str(args, "content", "")))
				if e != nil {
					return "", nil, e
				}
				return "wrote " + p, []string{p}, nil
			}
			return writeFile(h, str(args, "path", ""), str(args, "content", ""))
		}},
		{Spec{"patch_file", true, "Replace one exact text block.", `{"path":"string","old_text":"string","new_text":"string"}`}, func(ctx context.Context, h Host, args map[string]any) (string, []string, error) {
			if host, ok := h.(interface {
				PatchFile(context.Context, string, string, string) error
			}); ok {
				p := str(args, "path", "")
				if p == "" {
					return "", nil, errors.New("path required")
				}
				e := host.PatchFile(ctx, p, str(args, "old_text", ""), str(args, "new_text", ""))
				if e != nil {
					return "", nil, e
				}
				return "patched " + p, []string{p}, nil
			}
			return patchFile(h, str(args, "path", ""), str(args, "old_text", ""), str(args, "new_text", ""))
		}},
		{Spec{"todo_add", false, "Add a task to the todo ledger.", `{"text":"string"}`}, func(_ context.Context, h Host, args map[string]any) (string, []string, error) {
			return h.TodoAdd(str(args, "text", "")), nil, nil
		}},
		{Spec{"todo_update", false, "Update a todo status.", `{"id":"string","status":"string"}`}, func(_ context.Context, h Host, args map[string]any) (string, []string, error) {
			return "", nil, h.TodoUpdate(str(args, "id", ""), str(args, "status", ""))
		}},
		{Spec{"todo_list", false, "List todo entries.", `{}`}, func(_ context.Context, h Host, _ map[string]any) (string, []string, error) {
			return h.TodoList(), nil, nil
		}},
		{Spec{"agent", false, "Start an isolated worker agent.", `{"role":"string","prompt":"string"}`}, func(ctx context.Context, h Host, args map[string]any) (string, []string, error) {
			if host, ok := h.(interface {
				StartWorkerContext(context.Context, string, string) (string, error)
			}); ok {
				out, err := host.StartWorkerContext(ctx, str(args, "role", "explore"), str(args, "prompt", ""))
				return out, nil, err
			}
			out, err := h.StartWorker(str(args, "role", "explore"), str(args, "prompt", ""))
			return out, nil, err
		}},
		{Spec{"send_message", false, "Send a message to a worker.", `{"id":"string","message":"string"}`}, func(_ context.Context, h Host, args map[string]any) (string, []string, error) {
			return "message queued", nil, h.SendWorker(str(args, "id", ""), str(args, "message", ""))
		}},
		{Spec{"task_stop", false, "Stop a worker.", `{"id":"string"}`}, func(ctx context.Context, h Host, args map[string]any) (string, []string, error) {
			if host, ok := h.(interface {
				StopWorkerContext(context.Context, string) error
			}); ok {
				return "worker stopped", nil, host.StopWorkerContext(ctx, str(args, "id", ""))
			}
			return "worker stopped", nil, h.StopWorker(str(args, "id", ""))
		}},
		{Spec{"enter_plan_mode", false, "Enter plan mode.", `{"path":"string?"}`}, func(_ context.Context, h Host, args map[string]any) (string, []string, error) {
			out, err := h.EnterPlan(str(args, "path", ".pico/plans/active.md"))
			return out, nil, err
		}},
		{Spec{"exit_plan_mode", false, "Exit plan mode.", `{}`}, func(_ context.Context, h Host, _ map[string]any) (string, []string, error) {
			return h.ExitPlan(), nil, nil
		}},
		{Spec{"ask_user", false, "Ask the interactive user a question.", `{"question":"string"}`}, func(_ context.Context, h Host, args map[string]any) (string, []string, error) {
			return h.AskUser(str(args, "question", "")), nil, nil
		}},
	}
}
func (r *Registry) Specs() []Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Spec, 0, len(r.specs))
	for _, s := range r.specs {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (r *Registry) Spec(name string) (Spec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.specs[name]
	return s, ok
}

// Validate returns an owned JSON-normalized action. Approval and execution
// both use this value, not a mutable map supplied by the model or approver.
func (r *Registry) Validate(call model.ToolCall) (model.ToolCall, error) {
	r.mu.RLock()
	_, ok := r.handlers[call.Name]
	v := r.validators[call.Name]
	r.mu.RUnlock()
	if !ok {
		return call, fmt.Errorf("%w %q", ErrUnknownTool, call.Name)
	}
	return validateCall(call, v)
}

func validateCall(call model.ToolCall, v *jsonschema.Resolved) (model.ToolCall, error) {
	args := call.Args
	if args == nil {
		args = map[string]any{}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return call, err
	}
	var normalized map[string]any
	if err = json.Unmarshal(raw, &normalized); err != nil {
		return call, err
	}
	if v != nil {
		if err = v.Validate(normalized); err != nil {
			return call, err
		}
	}
	call.Args = normalized
	return call, nil
}

func (r *Registry) Run(ctx context.Context, h Host, call model.ToolCall) model.ToolResult {
	invocation, err := r.Prepare(call)
	if err != nil {
		code := "invalid_parameters"
		if errors.Is(err, ErrUnknownTool) {
			code = "unknown_tool"
		}
		return model.ToolResult{Name: call.Name, Status: "error", Error: err.Error(), ErrorCode: code}
	}
	return invocation.Run(ctx, h)
}

func inspectImage(ctx context.Context, h Host, args map[string]any) (string, []string, error) {
	question := str(args, "question", str(args, "prompt", "Describe the image and extract details relevant to the current coding task."))
	if profile := str(args, "profile", ""); profile != "" {
		question += "\nInspection profile: " + profile
	}
	if schema := str(args, "output_schema", ""); schema != "" {
		question += "\nReturn output matching this schema: " + schema
	}
	out, err := h.InspectImage(ctx, str(args, "path", ""), question)
	return out, nil, err
}

func runShell(parent context.Context, h Host, command string, seconds int) (string, []string, error) {
	if strings.TrimSpace(command) == "" {
		return "", nil, errors.New("command is required")
	}
	if err := safeCommand(command); err != nil {
		return "", nil, err
	}
	if seconds < 1 || seconds > 600 {
		return "", nil, errors.New("timeout must be in [1,600]")
	}
	out, err := h.ExecuteShell(parent, command, seconds)
	return out, nil, err
}
func safeCommand(command string) error {
	lower := strings.ToLower(command)
	blocked := []string{"rm -rf /", "rm -rf ~", "git reset --hard", "git clean -fd", "shutdown", "reboot", ":(){:|:&};:"}
	for _, b := range blocked {
		if strings.Contains(lower, b) {
			return fmt.Errorf("tool policy blocked command containing %q", b)
		}
	}
	return nil
}
func writeFile(h Host, rel, content string) (string, []string, error) {
	if rel == "" {
		return "", nil, errors.New("path is required")
	}
	p, err := h.Resolve(rel)
	if err != nil {
		return "", nil, err
	}
	if st, err := os.Stat(p); err == nil {
		if st.IsDir() {
			return "", nil, errors.New("path is a directory")
		}
		if !h.FreshRead(p) {
			return "", nil, fmt.Errorf("write_file requires a fresh read_file of %s before modifying it", rel)
		}
	}
	if err = os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", nil, err
	}
	if err = os.WriteFile(p, []byte(content), 0o644); err != nil {
		return "", nil, err
	}
	h.Invalidate(p)
	return fmt.Sprintf("wrote %d bytes to %s", len(content), rel), []string{h.Relative(p)}, nil
}
func patchFile(h Host, rel, oldText, newText string) (string, []string, error) {
	if rel == "" || oldText == "" {
		return "", nil, errors.New("path and old_text are required")
	}
	p, err := h.Resolve(rel)
	if err != nil {
		return "", nil, err
	}
	if !h.FreshRead(p) {
		return "", nil, fmt.Errorf("patch_file requires a fresh read_file of %s before modifying it", rel)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", nil, err
	}
	if bytes.Count(b, []byte(oldText)) != 1 {
		return "", nil, fmt.Errorf("old_text must occur exactly once, found %d", bytes.Count(b, []byte(oldText)))
	}
	updated := bytes.Replace(b, []byte(oldText), []byte(newText), 1)
	if err = os.WriteFile(p, updated, 0o644); err != nil {
		return "", nil, err
	}
	h.Invalidate(p)
	return "patched " + rel, []string{h.Relative(p)}, nil
}
func str(m map[string]any, k, def string) string {
	v, ok := m[k]
	if !ok || v == nil {
		return def
	}
	return fmt.Sprint(v)
}
func integer(m map[string]any, k string, def int) int {
	v, ok := m[k]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, err := strconv.Atoi(n)
		if err == nil {
			return i
		}
	}
	return def
}
func empty(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(empty)"
	}
	if len(s) > 12000 {
		return s[:12000] + "\n...[truncated]"
	}
	return s
}
