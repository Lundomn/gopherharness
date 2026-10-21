package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/contextbuilder"
	"github.com/Lundomn/gopherharness/internal/governance"
	"github.com/Lundomn/gopherharness/internal/mcpbridge"
	"github.com/Lundomn/gopherharness/internal/memory"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/permission"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/redact"
	"github.com/Lundomn/gopherharness/internal/sandbox"
	"github.com/Lundomn/gopherharness/internal/skills"
	"github.com/Lundomn/gopherharness/internal/state"
	"github.com/Lundomn/gopherharness/internal/store"
	"github.com/Lundomn/gopherharness/internal/tools"
	"github.com/Lundomn/gopherharness/internal/worker"
	"github.com/Lundomn/gopherharness/internal/workspace"
)

type Options struct {
	FallbackClients      []provider.Client
	ProviderRetries      int
	MemoryUser           string
	MemoryScope          string
	FactBackend          memory.Backend
	EnableFactExtraction bool
	SharedMemory         *memory.Manager
	SharedTodos          *state.TodoLedger
	MCPServers           []mcpbridge.Server
	Prices               map[string]provider.Price
	HistorySelector      contextbuilder.Selector
	Root                 string
	Config               config.Config
	Provider             provider.Client
	Vision               provider.VisionClient
	Resume               string
	Approval             permission.Policy
	Approver             permission.Approver
	MaxSteps             int
	MaxAttempts          int
	MaxNewTokens         int
	PromptBudget         int
	Sandbox              sandbox.Config
	AllowedTools         []string
	Secrets              []string
	FinalReadiness       governance.Mode
	DisableAutoDream     bool
	DreamInterval        time.Duration
	DreamMinSessions     int
	Input                io.Reader
	Output               io.Writer
	OnModelStart         func()
	OnDelta              func(string) error
}

type Agent struct {
	Facts            memory.Backend
	FactScope        memory.Scope
	MCP              *mcpbridge.Manager
	CallLedger       *provider.Ledger
	ownsLedger       bool
	traceLost        atomic.Uint64
	HistorySelector  contextbuilder.Selector
	Workspace        *workspace.Workspace
	Provider         provider.Client
	Vision           provider.VisionClient
	Session          *model.Session
	Sessions         *store.SessionStore
	Memory           *memory.Manager
	Todos            *state.TodoLedger
	Plan             *state.PlanController
	Workers          *worker.Manager
	Tools            *tools.Registry
	Skills           *skills.Registry
	Sandbox          *sandbox.Runner
	Redactor         *redact.Redactor
	FinalReadiness   governance.Mode
	allowedTools     map[string]bool
	Approval         permission.Policy
	Approver         permission.Approver
	MaxSteps         int
	MaxAttempts      int
	MaxNewTokens     int
	PromptBudget     int
	AutoDream        bool
	DreamInterval    time.Duration
	DreamMinSessions int
	Input            io.Reader
	Output           io.Writer
	OnModelStart     func()
	OnDelta          func(string) error
	Inbox            <-chan string
	// DrainInjected is called only before assembling the next model request.
	DrainInjected func() []string
	mu            sync.Mutex
	lastRun       string
	usageInput    int
	usageOutput   int
	modelCalls    int
	dreamWG       sync.WaitGroup
	dreamErrMu    sync.Mutex
	lastDreamErr  error
	ResumeStatus  string
}

func New(opts Options) (*Agent, error) {
	if opts.Root == "" {
		opts.Root = "."
	}
	w, err := workspace.Open(opts.Root)
	if err != nil {
		return nil, err
	}
	if opts.Provider == nil {
		p, err := opts.Config.Selected()
		if err != nil {
			return nil, err
		}
		opts.Provider, err = provider.New(p, 300)
		if err != nil {
			return nil, err
		}
	}
	visionProvided := opts.Vision != nil
	if opts.Vision == nil {
		opts.Vision, _ = opts.Provider.(provider.VisionClient)
	}
	if opts.Approval == "" {
		opts.Approval = permission.Ask
	}
	if opts.FinalReadiness == "" {
		opts.FinalReadiness = governance.Warn
	}
	if parsed, valid := governance.ParseMode(string(opts.FinalReadiness)); !valid {
		return nil, fmt.Errorf("invalid final readiness mode %q", opts.FinalReadiness)
	} else {
		opts.FinalReadiness = parsed
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 50
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = opts.MaxSteps + 2
	}
	if opts.MaxNewTokens <= 0 {
		opts.MaxNewTokens = 8192
	}
	if opts.PromptBudget <= 0 {
		opts.PromptBudget = 60000
	}
	dreamConfigured := opts.DreamInterval != 0 || opts.DreamMinSessions != 0
	if opts.DreamMinSessions <= 0 {
		opts.DreamMinSessions = 5
	}
	if !dreamConfigured {
		opts.DreamInterval = 24 * time.Hour
	}
	if opts.Input == nil {
		opts.Input = os.Stdin
	}
	if opts.Output == nil {
		opts.Output = os.Stdout
	}
	sessions := store.NewSessionStore(w.Root)
	resumeStatus := model.ResumeNone
	var resumeCheckpoint *model.Checkpoint
	var session *model.Session
	if opts.Resume != "" {
		session, err = sessions.Load(opts.Resume)
		if err != nil {
			return nil, fmt.Errorf("resume session: %w", err)
		}
		if checkpoint, checkpointErr := store.LoadLatestCheckpoint(w.Root, session.ID); checkpointErr == nil {
			resumeCheckpoint = checkpoint
			switch {
			case checkpoint.SchemaVersion != model.CheckpointSchema:
				resumeStatus = model.ResumeSchema
			case checkpoint.WorkspaceHash != w.Fingerprint():
				resumeStatus = model.ResumeWorkspace
			default:
				resumeStatus = model.ResumeFull
			}
		}
	} else {
		session = sessions.New(w.Root)
	}
	mem := opts.SharedMemory
	if mem == nil {
		mem, err = memory.OpenScoped(w.Root, session.ID+opts.MemoryScope)
		if err != nil {
			return nil, err
		}
	}
	if resumeStatus == model.ResumeFull && resumeCheckpoint != nil {
		mem.SetGoal(resumeCheckpoint.Goal)
		mem.SetNextStep(resumeCheckpoint.NextStep)
	}
	secrets := append([]string(nil), opts.Secrets...)
	for _, profile := range opts.Config.Providers {
		secrets = append(secrets, profile.APIKey)
	}
	a := &Agent{Workspace: w, Provider: opts.Provider, Vision: opts.Vision, Session: session, Sessions: sessions, Memory: mem, Todos: state.OpenTodo(w.Root), Plan: state.NewPlan(w.Root), Tools: tools.NewRegistry(), Skills: skills.Discover(w.Root), Sandbox: sandbox.New(opts.Sandbox), Redactor: redact.New(secrets...), FinalReadiness: opts.FinalReadiness, Approval: opts.Approval, Approver: opts.Approver, MaxSteps: opts.MaxSteps, MaxAttempts: opts.MaxAttempts, MaxNewTokens: opts.MaxNewTokens, PromptBudget: opts.PromptBudget, AutoDream: !opts.DisableAutoDream, DreamInterval: opts.DreamInterval, DreamMinSessions: opts.DreamMinSessions, Input: opts.Input, Output: opts.Output, OnModelStart: opts.OnModelStart, OnDelta: opts.OnDelta, ResumeStatus: resumeStatus, allowedTools: map[string]bool{}}
	for _, name := range opts.AllowedTools {
		a.allowedTools[name] = true
	}
	a.Workers = worker.New(a.runWorker)
	if opts.SharedTodos != nil {
		a.Todos = opts.SharedTodos
	}
	a.HistorySelector = opts.HistorySelector
	// One shared wrapper covers main calls, vision, workers and Dream. Child
	// agents borrow it and never close the parent's ledger.
	if metered, ok := a.Provider.(*provider.Metered); ok {
		a.CallLedger = metered.Ledger
	} else {
		ledger, ledgerErr := provider.OpenLedger(filepath.Join(w.Root, ".pico", "telemetry", session.ID+".calls.jsonl"), 256)
		if ledgerErr != nil {
			return nil, ledgerErr
		}
		a.CallLedger = ledger
		a.ownsLedger = true
		wrapped := provider.Meter(a.Provider, ledger, opts.Prices)
		a.Provider = wrapped
		if !visionProvided && a.Vision != nil {
			a.Vision = wrapped
		}
		if visionProvided {
			if client, ok := opts.Vision.(provider.Client); ok {
				a.Vision = provider.Meter(client, ledger, opts.Prices)
			}
		}
	}
	a.Facts = opts.FactBackend
	if opts.ProviderRetries > 0 || len(opts.FallbackClients) > 0 {
		clients := []provider.Client{a.Provider}
		for _, c := range opts.FallbackClients {
			clients = append(clients, provider.Meter(c, a.CallLedger, opts.Prices))
		}
		a.Provider = &provider.Resilient{Clients: clients, Retries: min(3, max(0, opts.ProviderRetries)), Delay: 200 * time.Millisecond}
		if !visionProvided && a.Vision != nil {
			a.Vision = a.Provider.(provider.VisionClient)
		}
	}
	user := opts.MemoryUser
	if user == "" {
		user = "local"
	}
	a.FactScope = memory.Scope{Repo: w.Root, User: user}
	if a.Facts == nil {
		var extractor memory.Extractor
		if opts.EnableFactExtraction {
			extractor = memory.ModelExtractor(func(ctx context.Context, prompt string, n int) (string, error) {
				c, e := a.Provider.Complete(ctx, prompt, n)
				return c.Text, e
			})
		}
		a.Facts = memory.OpenBackend(w.Root, extractor)
	}
	if err := a.saveSession(); err != nil {
		if a.ownsLedger {
			_ = a.CallLedger.Close()
		}
		return nil, err
	}
	if len(opts.MCPServers) > 0 {
		if opts.Sandbox.Mode != "" && opts.Sandbox.Mode != "off" {
			for _, s := range opts.MCPServers {
				if s.Transport == "stdio" {
					_ = a.Close()
					return nil, fmt.Errorf("MCP stdio in sandbox requires a sandbox process transport; refusing host fallback")
				}
			}
		}
		connectCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		a.MCP, err = mcpbridge.Connect(connectCtx, a.Tools, opts.MCPServers)
		cancel()
		if err != nil {
			_ = a.Close()
			return nil, err
		}
	}
	return a, nil
}

func (a *Agent) Ask(ctx context.Context, request string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.run(ctx, request, true)
}
func (a *Agent) SessionID() string      { return a.Session.ID }
func (a *Agent) LastRunID() string      { return a.lastRun }
func (a *Agent) Usage() (int, int, int) { return a.usageInput, a.usageOutput, a.modelCalls }
func (a *Agent) TracingLost() uint64    { return a.traceLost.Load() }
func (a *Agent) Reset() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Session = a.Sessions.New(a.Workspace.Root)
	a.ResumeStatus = model.ResumeNone
	return a.saveSession()
}

func (a *Agent) Close() error {
	if a.Workers != nil {
		a.Workers.Close()
	}
	dreamErr := a.WaitForMemoryMaintenance()
	if a.MCP != nil {
		dreamErr = errors.Join(dreamErr, a.MCP.Close())
	}
	if a.CallLedger != nil && a.ownsLedger {
		return errors.Join(dreamErr, a.CallLedger.Close())
	}
	return dreamErr
}

func (a *Agent) saveSession() error {
	snapshot := *a.Session
	snapshot.Messages = append([]model.Message(nil), a.Session.Messages...)
	if a.Redactor != nil {
		for index := range snapshot.Messages {
			snapshot.Messages[index].Content = a.Redactor.Text(snapshot.Messages[index].Content)
			snapshot.Messages[index].ToolCalls = append([]model.ToolCall(nil), snapshot.Messages[index].ToolCalls...)
			for j := range snapshot.Messages[index].ToolCalls {
				snapshot.Messages[index].ToolCalls[j].Args = a.Redactor.Map(snapshot.Messages[index].ToolCalls[j].Args)
			}
		}
		snapshot.Memory = a.Redactor.Map(a.Session.Memory)
	}
	if err := a.Sessions.Save(&snapshot); err != nil {
		return err
	}
	a.Session.UpdatedAt = snapshot.UpdatedAt
	return nil
}

func (a *Agent) runWorker(ctx context.Context, role, prompt string, inbox <-chan string) (string, error) {
	childSession := a.Sessions.New(a.Workspace.Root)
	child := &Agent{Workspace: a.Workspace, Provider: a.Provider, Vision: a.Vision, Session: childSession, Sessions: a.Sessions, Memory: a.Memory, Todos: a.Todos, Plan: state.NewPlan(a.Workspace.Root), Tools: tools.NewRegistry(), Skills: a.Skills, Sandbox: a.Sandbox, Redactor: a.Redactor, FinalReadiness: a.FinalReadiness, Approval: permission.Never, MaxSteps: 8, MaxAttempts: 10, MaxNewTokens: a.MaxNewTokens, PromptBudget: a.PromptBudget, Input: strings.NewReader(""), Output: io.Discard, Inbox: inbox, ResumeStatus: model.ResumeNone, allowedTools: a.allowedTools}
	child.Workers = worker.New(func(context.Context, string, string, <-chan string) (string, error) {
		return "", fmt.Errorf("nested workers are disabled")
	})
	child.Facts, child.FactScope = a.Facts, a.FactScope
	if mem, err := memory.OpenScoped(a.Workspace.Root, childSession.ID); err == nil {
		child.Memory = mem
	} else {
		return "", err
	}
	child.HistorySelector = a.HistorySelector
	return child.run(ctx, fmt.Sprintf("You are a %s worker. Stay within that role.\n\n%s", role, prompt), true)
}

func (a *Agent) drainInbox() int {
	count := 0
	if a.DrainInjected != nil {
		for _, message := range a.DrainInjected() {
			if message = strings.TrimSpace(message); message != "" {
				a.Session.Messages = append(a.Session.Messages, model.Message{Role: "user", Content: "Follow-up: " + message, CreatedAt: time.Now().UTC()})
				count++
			}
		}
	}
	if a.Inbox == nil {
		return count
	}
	for {
		select {
		case message, ok := <-a.Inbox:
			if !ok {
				return count
			}
			message = strings.TrimSpace(message)
			if message != "" {
				a.Session.Messages = append(a.Session.Messages, model.Message{Role: "user", Content: "Worker follow-up: " + message, CreatedAt: time.Now().UTC()})
				count++
			}
		default:
			return count
		}
	}
}

// tools.Host implementation.
func (a *Agent) Resolve(path string) (string, error) { return a.Workspace.Resolve(path) }
func (a *Agent) Relative(path string) string         { return a.Workspace.Relative(path) }
func (a *Agent) MarkRead(path string)                { a.Workspace.MarkRead(path) }
func (a *Agent) FreshRead(path string) bool          { return a.Workspace.FreshRead(path) }
func (a *Agent) Invalidate(path string) {
	a.Workspace.Invalidate(path)
	a.Memory.Invalidate(a.Relative(path))
}
func (a *Agent) RootDir() string            { return a.Workspace.Root }
func (a *Agent) ShellEnvironment() []string { return workspace.ShellEnv() }
func (a *Agent) ExecuteShell(ctx context.Context, command string, seconds int) (string, error) {
	result, err := a.Sandbox.Run(ctx, command, a.Workspace.Root, a.ShellEnvironment(), seconds)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("exit_code: %d\nsandbox_backend: %s\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Backend, outputOrEmpty(result.Stdout), outputOrEmpty(result.Stderr)), nil
}
func (a *Agent) InspectImage(ctx context.Context, path, prompt string) (string, error) {
	p, err := a.Workspace.Resolve(path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("image path is a directory")
	}
	if st.Size() > 20<<20 {
		return "", fmt.Errorf("image exceeds the 20 MiB limit")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return "", fmt.Errorf("unsupported image content type %q", mime)
	}
	if a.Vision == nil {
		return "", fmt.Errorf("provider %s does not support image input", a.Provider.Name())
	}
	completion, err := a.Vision.InspectImage(ctx, mime, data, prompt, min(a.MaxNewTokens, 4096))
	if err != nil {
		return "", err
	}
	a.modelCalls++
	a.usageInput += completion.Usage.InputTokens
	a.usageOutput += completion.Usage.OutputTokens
	return fmt.Sprintf("image: %s\ncontent_type: %s\nsize_bytes: %d\nanalysis:\n%s", a.Workspace.Relative(p), mime, len(data), completion.Text), nil
}
func (a *Agent) TodoAdd(text string) string            { return a.Todos.Add(text) }
func (a *Agent) TodoUpdate(id, status string) error    { return a.Todos.Update(id, status) }
func (a *Agent) TodoList() string                      { return a.Todos.List() }
func (a *Agent) EnterPlan(path string) (string, error) { return a.Plan.Enter(path) }
func (a *Agent) ExitPlan() string                      { return a.Plan.Exit() }
func (a *Agent) StartWorker(role, prompt string) (string, error) {
	return a.Workers.Start(role, prompt)
}
func (a *Agent) SendWorker(id, message string) error { return a.Workers.Send(id, message) }
func (a *Agent) StopWorker(id string) error          { return a.Workers.Stop(id) }
func (a *Agent) AskUser(question string) string {
	fmt.Fprintf(a.Output, "\n%s ", question)
	line, _ := bufio.NewReader(a.Input).ReadString('\n')
	return strings.TrimSpace(line)
}

func outputOrEmpty(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "(empty)"
	}
	if len(value) > 12000 {
		return value[:12000] + "\n...[truncated]"
	}
	return value
}

func (a *Agent) toolAllowed(name string) bool {
	return len(a.allowedTools) == 0 || a.allowedTools[name]
}
func (a *Agent) activeToolSpecs() []tools.Spec {
	all := a.Tools.Specs()
	if len(a.allowedTools) == 0 {
		return all
	}
	out := make([]tools.Spec, 0, len(all))
	for _, spec := range all {
		if a.allowedTools[spec.Name] {
			out = append(out, spec)
		}
	}
	return out
}

func (a *Agent) planAllows(call model.ToolCall) error {
	active, path := a.Plan.Active()
	if !active {
		return nil
	}
	if call.Name != "write_file" && call.Name != "patch_file" && call.Name != "run_shell" {
		if spec, ok := a.Tools.Spec(call.Name); ok && spec.Risky {
			return fmt.Errorf("plan mode blocks external side effects")
		}
		return nil
	}
	if call.Name == "run_shell" {
		return fmt.Errorf("plan mode blocks shell execution")
	}
	target := fmt.Sprint(call.Args["path"])
	if filepath.Clean(target) != filepath.Clean(path) {
		return fmt.Errorf("plan mode only allows writes to %s", path)
	}
	return nil
}
