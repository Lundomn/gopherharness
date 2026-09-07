package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Lundomn/gopherharness/internal/model"
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
	specs    map[string]Spec
	handlers map[string]Handler
}

func NewRegistry() *Registry {
	r := &Registry{specs: map[string]Spec{}, handlers: map[string]Handler{}}
	for _, definition := range builtinDefinitions() {
		_ = r.Register(definition)
	}
	return r
}

func (r *Registry) Register(definition Definition) error {
	name := strings.TrimSpace(definition.Spec.Name)
	if name == "" || definition.Handler == nil {
		return errors.New("tool definition requires a name and handler")
	}
	if _, exists := r.specs[name]; exists {
		return fmt.Errorf("tool %q is already registered", name)
	}
	definition.Spec.Name = name
	r.specs[name] = definition.Spec
	r.handlers[name] = definition.Handler
	return nil
}

func builtinDefinitions() []Definition {
	return []Definition{
		{Spec{"list_files", false, "List files in the workspace.", `{"path":"string?"}`}, func(_ context.Context, h Host, args map[string]any) (string, []string, error) {
			return listFiles(h, str(args, "path", "."))
		}},
		{Spec{"read_file", false, "Read a UTF-8 file by line range.", `{"path":"string","start":"int?","end":"int?"}`}, func(_ context.Context, h Host, args map[string]any) (string, []string, error) {
			return readFile(h, str(args, "path", ""), integer(args, "start", 1), integer(args, "end", 200))
		}},
		{Spec{"search", false, "Search workspace text using a regular expression.", `{"pattern":"string","path":"string?"}`}, func(_ context.Context, h Host, args map[string]any) (string, []string, error) {
			return search(h, str(args, "pattern", ""), str(args, "path", "."))
		}},
		{Spec{"inspect_image", false, "Understand an image with the configured vision model.", `{"path":"string","question":"string?","profile":"string?","output_schema":"string?"}`}, inspectImage},
		{Spec{"run_shell", true, "Run a shell command in the workspace.", `{"command":"string","timeout":"int?"}`}, func(ctx context.Context, h Host, args map[string]any) (string, []string, error) {
			return runShell(ctx, h, str(args, "command", ""), integer(args, "timeout", 20))
		}},
		{Spec{"write_file", true, "Write a text file.", `{"path":"string","content":"string"}`}, func(_ context.Context, h Host, args map[string]any) (string, []string, error) {
			return writeFile(h, str(args, "path", ""), str(args, "content", ""))
		}},
		{Spec{"patch_file", true, "Replace one exact text block.", `{"path":"string","old_text":"string","new_text":"string"}`}, func(_ context.Context, h Host, args map[string]any) (string, []string, error) {
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
		{Spec{"agent", false, "Start an isolated worker agent.", `{"role":"string","prompt":"string"}`}, func(_ context.Context, h Host, args map[string]any) (string, []string, error) {
			out, err := h.StartWorker(str(args, "role", "explore"), str(args, "prompt", ""))
			return out, nil, err
		}},
		{Spec{"send_message", false, "Send a message to a worker.", `{"id":"string","message":"string"}`}, func(_ context.Context, h Host, args map[string]any) (string, []string, error) {
			return "message queued", nil, h.SendWorker(str(args, "id", ""), str(args, "message", ""))
		}},
		{Spec{"task_stop", false, "Stop a worker.", `{"id":"string"}`}, func(_ context.Context, h Host, args map[string]any) (string, []string, error) {
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
	out := make([]Spec, 0, len(r.specs))
	for _, s := range r.specs {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (r *Registry) Spec(name string) (Spec, bool) { s, ok := r.specs[name]; return s, ok }

func (r *Registry) Run(ctx context.Context, h Host, call model.ToolCall) model.ToolResult {
	result := model.ToolResult{Name: call.Name, Status: "ok", Metadata: map[string]any{}}
	handler, ok := r.handlers[call.Name]
	if !ok {
		result.Status = "error"
		result.Error = fmt.Sprintf("unknown tool %q", call.Name)
		result.ErrorCode = "unknown_tool"
		return result
	}
	output, paths, err := handler(ctx, h, call.Args)
	if err != nil {
		result.Status = "error"
		result.Error = err.Error()
		if strings.Contains(err.Error(), "escapes workspace") {
			result.ErrorCode = "path_escape"
		}
		return result
	}
	result.Output = output
	result.AffectedPaths = paths
	return result
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

func listFiles(h Host, rel string) (string, []string, error) {
	p, err := h.Resolve(rel)
	if err != nil {
		return "", nil, err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return "", nil, err
	}
	var lines []string
	for i, e := range entries {
		if i >= 200 {
			break
		}
		kind := "[F]"
		if e.IsDir() {
			kind = "[D]"
		}
		lines = append(lines, fmt.Sprintf("%s %s", kind, h.Relative(filepath.Join(p, e.Name()))))
	}
	if len(lines) == 0 {
		return "(empty)", nil, nil
	}
	return strings.Join(lines, "\n"), nil, nil
}
func readFile(h Host, rel string, start, end int) (string, []string, error) {
	if rel == "" {
		return "", nil, errors.New("path is required")
	}
	if start < 1 || end < start {
		return "", nil, errors.New("invalid line range")
	}
	p, err := h.Resolve(rel)
	if err != nil {
		return "", nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	var lines []string
	s := bufio.NewScanner(f)
	n := 0
	for s.Scan() {
		n++
		if n >= start && n <= end {
			lines = append(lines, fmt.Sprintf("%4d: %s", n, s.Text()))
		}
		if n > end {
			break
		}
	}
	if err = s.Err(); err != nil {
		return "", nil, err
	}
	h.MarkRead(p)
	return "# " + h.Relative(p) + "\n" + strings.Join(lines, "\n"), nil, nil
}
func search(h Host, pattern, rel string) (string, []string, error) {
	if pattern == "" {
		return "", nil, errors.New("pattern is required")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", nil, err
	}
	p, err := h.Resolve(rel)
	if err != nil {
		return "", nil, err
	}
	var matches []string
	walk := func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".pico" || d.Name() == ".venv" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if len(matches) >= 200 {
			return filepath.SkipAll
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		s := bufio.NewScanner(f)
		n := 0
		for s.Scan() {
			n++
			if re.MatchString(s.Text()) {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", h.Relative(path), n, s.Text()))
				if len(matches) >= 200 {
					break
				}
			}
		}
		return nil
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", nil, err
	}
	if st.IsDir() {
		err = filepath.WalkDir(p, walk)
	} else {
		err = walk(p, dirEntry{st}, nil)
	}
	if err != nil && err != filepath.SkipAll {
		return "", nil, err
	}
	if len(matches) == 0 {
		return "(no matches)", nil, nil
	}
	return strings.Join(matches, "\n"), nil, nil
}

type dirEntry struct{ fs.FileInfo }

func (d dirEntry) Type() fs.FileMode          { return d.Mode().Type() }
func (d dirEntry) Info() (fs.FileInfo, error) { return d.FileInfo, nil }
func runShell(parent context.Context, h Host, command string, seconds int) (string, []string, error) {
	if strings.TrimSpace(command) == "" {
		return "", nil, errors.New("command is required")
	}
	if err := safeCommand(command); err != nil {
		return "", nil, err
	}
	if seconds < 1 || seconds > 120 {
		return "", nil, errors.New("timeout must be in [1,120]")
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
