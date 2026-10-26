package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	FallbackClients       []provider.Client
	ProviderRetries       int
	CostLimitNanoUSD      int64
	SharedBudget          *provider.BudgetAccount
	MemoryUser            string
	MemoryScope           string
	FactBackend           memory.Backend
	EnableFactExtraction  bool
	DisablePersonalSkills bool
	AllowWorkerWrites     bool
	SharedMemory          *memory.Manager
	SharedTodos           *state.TodoLedger
	MCPServers            []mcpbridge.Server
	Prices                map[string]provider.Price
	HistorySelector       contextbuilder.Selector
	Root                  string
	Config                config.Config
	Provider              provider.Client
	Vision                provider.VisionClient
	Resume                string
	Approval              permission.Policy
	Approver              permission.Approver
	AsyncApproval         bool
	MaxSteps              int
	MaxAttempts           int
	MaxNewTokens          int
	PromptBudget          int
	ContextWindowTokens   int
	Tokenizer             string
	TokenCounter          contextbuilder.Counter
	HistorySelection      string // model (default) or rule; explicit Selector takes precedence
	Sandbox               sandbox.Config
	AllowedTools          []string
	Secrets               []string
	FinalReadiness        governance.Mode
	DisableAutoDream      bool
	DreamInterval         time.Duration
	DreamMinSessions      int
	Input                 io.Reader
	Output                io.Writer
	OnModelStart          func()
	OnDelta               func(string) error
}

type Agent struct {
	Facts               memory.Backend
	FactWriter          *memory.Writer
	ownsFactWriter      bool
	ownsFacts           bool
	FactScope           memory.Scope
	MCP                 *mcpbridge.Manager
	CallLedger          *provider.Ledger
	CostBudget          *provider.BudgetAccount
	ownsLedger          bool
	traceLost           atomic.Uint64
	HistorySelector     contextbuilder.Selector
	curatorEnabled      bool
	Workspace           *workspace.Workspace
	Provider            provider.Client
	Vision              provider.VisionClient
	Session             *model.Session
	Sessions            *store.SessionStore
	Memory              *memory.Manager
	Todos               *state.TodoLedger
	Plan                *state.PlanController
	Workers             *worker.Manager
	Tools               *tools.Registry
	Skills              *skills.Registry
	Sandbox             *sandbox.Runner
	Redactor            *redact.Redactor
	FinalReadiness      governance.Mode
	allowedTools        map[string]bool
	Approval            permission.Policy
	Approver            permission.Approver
	AsyncApproval       bool
	AllowWorkerWrites   bool
	MaxSteps            int
	MaxAttempts         int
	MaxNewTokens        int
	PromptBudget        int
	ContextWindowTokens int
	TokenCounter        contextbuilder.Counter
	AutoDream           bool
	DreamInterval       time.Duration
	DreamMinSessions    int
	Input               io.Reader
	Output              io.Writer
	OnModelStart        func()
	OnDelta             func(string) error
	Inbox               <-chan string
	// DrainInjected is called only before assembling the next model request.
	DrainInjected   func() []string
	mu              contextMutex
	lastRun         string
	usageInput      int
	usageOutput     int
	modelCalls      int
	dreamWG         sync.WaitGroup
	dreamErrMu      sync.Mutex
	lastDreamErr    error
	ResumeStatus    string
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	closed          atomic.Bool
	closeOnce       sync.Once
	closeErr        error
}

func New(opts Options) (*Agent, error) {
	if opts.Root == "" {
		opts.Root = "."
	}
	if opts.MemoryUser == "" {
		opts.MemoryUser = "local"
	}
	w, err := workspace.Open(opts.Root)
	if err != nil {
		return nil, err
	}
	if err := applyProviderConfig(&opts); err != nil {
		return nil, err
	}
	if opts.Sandbox.Mode == "" && opts.Config.Sandbox.Mode != "" {
		opts.Sandbox = sandbox.Config{Mode: opts.Config.Sandbox.Mode, Backend: opts.Config.Sandbox.Backend, WorkspaceWrite: opts.Config.Sandbox.WorkspaceWrite, DenyNetwork: opts.Config.Sandbox.DenyNetwork}
	}
	if err := sandbox.New(opts.Sandbox).Validate(); err != nil {
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
	if opts.AllowWorkerWrites && opts.Approval != permission.Auto {
		return nil, fmt.Errorf("worker writes require explicit auto approval on the parent")
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
		opts.MaxAttempts = 40
	}
	if err := resolveContextPolicy(&opts); err != nil {
		return nil, err
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
		owner := session.MemoryUser
		if owner == "" {
			owner = "local"
		}
		if owner != opts.MemoryUser {
			return nil, fmt.Errorf("session belongs to another memory user")
		}
		if checkpoint, checkpointErr := store.LoadLatestCheckpoint(w.Root, session.ID); checkpointErr == nil {
			resumeCheckpoint = checkpoint
			switch {
			case checkpoint.SchemaVersion != model.CheckpointSchema:
				resumeStatus = model.ResumeSchema
			case checkpoint.WorkspaceHash == "" || checkpoint.WorkspaceHash != w.Fingerprint():
				resumeStatus = model.ResumeWorkspace
			default:
				resumeStatus = model.ResumeFull
			}
		}
	} else {
		session = sessions.New(w.Root)
	}
	session.MemoryUser = opts.MemoryUser
	if opts.Resume != "" {
		pending, e := activeApprovalContinuation(w.Root, session.ID)
		if e != nil {
			return nil, e
		}
		if !pending {
			closed, _, e := closeInterruptedExchanges(session.Messages)
			if e != nil {
				return nil, e
			}
			session.Messages = closed
		}
	}
	mem := opts.SharedMemory
	if mem == nil {
		mem, err = memory.OpenScopedUser(w.Root, session.ID+opts.MemoryScope, opts.MemoryUser)
		if err != nil {
			return nil, err
		}
	}
	secrets := append([]string(nil), opts.Secrets...)
	secrets = append(secrets, opts.Config.Memory.APIKey)
	for _, profile := range opts.Config.Providers {
		secrets = append(secrets, profile.APIKey)
	}
	a := &Agent{Workspace: w, Provider: opts.Provider, Vision: opts.Vision, Session: session, Sessions: sessions, Memory: mem, Todos: state.OpenTodo(w.Root), Plan: state.NewPlan(w.Root), Tools: tools.NewRegistry(), Sandbox: sandbox.New(opts.Sandbox), Redactor: redact.New(secrets...), FinalReadiness: opts.FinalReadiness, Approval: opts.Approval, Approver: opts.Approver, MaxSteps: opts.MaxSteps, MaxAttempts: opts.MaxAttempts, MaxNewTokens: opts.MaxNewTokens, PromptBudget: opts.PromptBudget, AutoDream: !opts.DisableAutoDream, DreamInterval: opts.DreamInterval, DreamMinSessions: opts.DreamMinSessions, Input: opts.Input, Output: opts.Output, OnModelStart: opts.OnModelStart, OnDelta: opts.OnDelta, ResumeStatus: resumeStatus, allowedTools: map[string]bool{}}
	a.Skills = skills.DiscoverScoped(w.Root, !opts.DisablePersonalSkills)
	a.AsyncApproval = opts.AsyncApproval
	a.AllowWorkerWrites = opts.AllowWorkerWrites
	a.FactScope = memory.Scope{Repo: w.Root, User: opts.MemoryUser}
	a.ContextWindowTokens, a.TokenCounter = opts.ContextWindowTokens, opts.TokenCounter
	a.lifecycleCtx, a.lifecycleCancel = context.WithCancel(context.Background())
	for _, name := range opts.AllowedTools {
		a.allowedTools[name] = true
	}
	a.Workers, err = a.newWorkerManager()
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	if opts.Resume != "" {
		// Acquire worker/session ownership before advancing the epoch. A failed
		// second owner must not invalidate a still-running first owner.
		claimCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = sessions.Claim(claimCtx, session)
		cancel()
		if err != nil {
			_ = a.Close()
			return nil, err
		}
	}
	if resumeStatus == model.ResumeFull && resumeCheckpoint != nil {
		mem.SetGoal(resumeCheckpoint.Goal)
		mem.SetNextStep(resumeCheckpoint.NextStep)
	}
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
			_ = a.Close()
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
	a.CostBudget = opts.SharedBudget
	if a.CostBudget == nil && opts.CostLimitNanoUSD > 0 {
		a.CostBudget, err = provider.OpenBudget(context.Background(), filepath.Join(w.Root, ".pico", "budgets", session.ID+".json"), opts.CostLimitNanoUSD)
		if err != nil {
			_ = a.Close()
			return nil, err
		}
	}
	wrapBudget := func(client provider.Client) (provider.Client, error) {
		if a.CostBudget == nil {
			return client, nil
		}
		prices, e := provider.FixedPrices(opts.Prices)
		if e != nil {
			return nil, e
		}
		return provider.WrapBudget(client, a.CostBudget, prices)
	}
	a.Provider, err = wrapBudget(a.Provider)
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	if a.Vision != nil {
		if !visionProvided {
			a.Vision, _ = a.Provider.(provider.VisionClient)
		} else if a.CostBudget != nil {
			if client, ok := a.Vision.(provider.Client); ok {
				wrapped, e := wrapBudget(client)
				if e != nil {
					_ = a.Close()
					return nil, e
				}
				a.Vision = wrapped.(provider.VisionClient)
			} else {
				_ = a.Close()
				return nil, fmt.Errorf("budgeted vision requires a metered provider client")
			}
		}
	}
	if opts.ProviderRetries > 0 || len(opts.FallbackClients) > 0 {
		clients := []provider.Client{a.Provider}
		for _, c := range opts.FallbackClients {
			wrapped, e := wrapBudget(provider.Meter(c, a.CallLedger, opts.Prices))
			if e != nil {
				_ = a.Close()
				return nil, e
			}
			clients = append(clients, wrapped)
		}
		a.Provider = &provider.Resilient{Clients: clients, Retries: min(3, max(0, opts.ProviderRetries)), Delay: time.Second}
		if !visionProvided && a.Vision != nil {
			a.Vision = a.Provider.(provider.VisionClient)
		}
	}
	a.curatorEnabled = a.HistorySelector == nil && opts.HistorySelection != "rule"
	user := opts.MemoryUser
	if user == "" {
		user = "local"
	}
	a.FactScope = memory.Scope{Repo: w.Root, User: user}
	if a.Facts == nil && opts.Config.Memory.BaseURL != "" {
		a.Facts, err = memory.NewHTTPBackend(memory.HTTPOptions{BaseURL: opts.Config.Memory.BaseURL, APIKey: opts.Config.Memory.APIKey, Timeout: time.Duration(opts.Config.Memory.TimeoutSeconds) * time.Second})
		if err != nil {
			_ = a.Close()
			return nil, err
		}
		a.ownsFacts = true
	}
	if a.Facts == nil {
		var extractor memory.Extractor
		if opts.EnableFactExtraction || opts.Config.Memory.ExtractFacts {
			extractor = memory.ModelExtractor(func(ctx context.Context, prompt string, n int) (string, error) {
				c, e := a.Provider.Complete(ctx, prompt, n)
				return c.Text, e
			})
		}
		a.Facts = memory.OpenBackend(w.Root, extractor)
		a.ownsFacts = true
	}
	a.FactWriter, err = memory.NewWriter(w.Root, a.FactScope, a.Facts, memory.WriterOptions{})
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	a.ownsFactWriter = true
	if err := a.saveSession(); err != nil {
		_ = a.Close()
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if a.lifecycleCtx != nil {
		stop := context.AfterFunc(a.lifecycleCtx, cancel)
		defer stop()
	}
	if err := a.mu.LockContext(ctx); err != nil {
		return "", err
	}
	defer a.mu.Unlock()
	if a.closed.Load() {
		return "", fmt.Errorf("agent is closed")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	release, err := a.Sessions.TurnLease(ctx, a.Session.ID)
	if err != nil {
		return "", err
	}
	defer release()
	if err := a.ensureNoPendingApproval(ctx); err != nil {
		return "", err
	}
	return a.run(ctx, request, true)
}
func (a *Agent) SessionID() string { return a.Session.ID }
func (a *Agent) ReadSkill(name string) (string, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "local/")
	s, ok := a.Skills.Get(name)
	if !ok {
		return "", fmt.Errorf("unknown registered skill %q", name)
	}
	return fmt.Sprintf("Skill: %s\nSkill file: %s\n\n%s", s.Name, s.Path, s.Body), nil
}
func (a *Agent) LastRunID() string      { return a.lastRun }
func (a *Agent) Usage() (int, int, int) { return a.usageInput, a.usageOutput, a.modelCalls }
func (a *Agent) TracingLost() uint64    { return a.traceLost.Load() }
func (a *Agent) Reset() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed.Load() {
		return fmt.Errorf("agent is closed")
	}
	next := a.Sessions.New(a.Workspace.Root)
	next.MemoryUser = a.FactScope.User
	mem, err := memory.OpenScopedUser(a.Workspace.Root, next.ID, a.FactScope.User)
	if err != nil {
		return err
	}
	workers, err := worker.NewDurable(a.runWorker, worker.Options{Root: a.Workspace.Root, Session: next.ID, Principal: a.FactScope.User, Redact: a.Redactor.Text, Lifetime: a.lifecycleCtx})
	if err != nil {
		return err
	}
	if err := a.Sessions.Save(next); err != nil {
		workers.Close()
		return err
	}
	// Prepare the replacement fully before closing the old owner. Failed reset
	// must not leave the live agent with a closed manager and half-new session.
	a.Workers.Close()
	_ = a.WaitForMemoryMaintenance()
	a.Session = next
	a.Workers = workers
	a.Memory = mem
	a.ResumeStatus = model.ResumeNone
	return nil
}

func (a *Agent) Close() error {
	a.closeOnce.Do(func() {
		a.closed.Store(true)
		if a.lifecycleCancel != nil {
			a.lifecycleCancel()
		}
		if a.Workers != nil {
			a.Workers.Seal()
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.Workers != nil {
			a.Workers.Close()
			a.closeErr = errors.Join(a.closeErr, a.Workers.NotificationError())
		}
		dreamErr := a.WaitForMemoryMaintenance()
		if a.ownsFactWriter && a.FactWriter != nil {
			dreamErr = errors.Join(dreamErr, a.FactWriter.Close())
		}
		if a.ownsFacts && a.Facts != nil {
			dreamErr = errors.Join(dreamErr, a.Facts.Close())
		}
		if a.MCP != nil {
			dreamErr = errors.Join(dreamErr, a.MCP.Close())
		}
		if a.CallLedger != nil && a.ownsLedger {
			dreamErr = errors.Join(dreamErr, a.CallLedger.Close())
		}
		a.closeErr = errors.Join(a.closeErr, dreamErr)
	})
	return a.closeErr
}

func (a *Agent) saveSession() error {
	snapshot := *a.Session
	snapshot.Messages = append([]model.Message(nil), a.Session.Messages...)
	if a.Redactor != nil {
		for index := range snapshot.Messages {
			for _, item := range snapshot.Messages[index].NativeContent {
				var decoded any
				if err := json.Unmarshal(item.Data, &decoded); err != nil {
					return fmt.Errorf("invalid native context block")
				}
				original, _ := json.Marshal(decoded)
				safe, _ := json.Marshal(a.Redactor.Value(decoded))
				if !bytes.Equal(original, safe) {
					// Signed/opaque blocks must not be edited and replayed with a
					// broken signature. Refuse persistence rather than leak secrets.
					return fmt.Errorf("native context contains sensitive content; refusing to persist or execute its tool batch")
				}
			}
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
	a.Session.Revision, a.Session.Epoch, a.Session.Fence = snapshot.Revision, snapshot.Epoch, snapshot.Fence
	return nil
}

func (a *Agent) runWorker(ctx context.Context, role, prompt string, inbox <-chan string) (string, error) {
	childSession := a.Sessions.New(a.Workspace.Root)
	childSession.MemoryUser = a.FactScope.User
	child := &Agent{Workspace: a.Workspace, Provider: a.Provider, Vision: a.Vision, Session: childSession, Sessions: a.Sessions, Memory: a.Memory, Todos: a.Todos, Plan: state.NewPlan(a.Workspace.Root), Tools: tools.NewRegistry(), Skills: a.Skills, Sandbox: a.Sandbox, Redactor: a.Redactor, FinalReadiness: a.FinalReadiness, Approval: permission.Never, MaxSteps: 8, MaxAttempts: 10, MaxNewTokens: a.MaxNewTokens, PromptBudget: a.PromptBudget, Input: strings.NewReader(""), Output: io.Discard, Inbox: inbox, ResumeStatus: model.ResumeNone, allowedTools: a.allowedTools}
	child.ContextWindowTokens, child.TokenCounter = a.ContextWindowTokens, a.TokenCounter
	if a.AllowWorkerWrites {
		child.Approval = permission.Auto
	}
	child.Workspace = a.Workspace.View()
	child.lifecycleCtx, child.lifecycleCancel = context.WithCancel(ctx)
	child.MaxSteps, child.MaxAttempts = 50, 15
	child.curatorEnabled = a.curatorEnabled
	child.FactWriter = a.FactWriter
	child.CostBudget = a.CostBudget
	child.Workers = worker.New(func(context.Context, string, string, <-chan string) (string, error) {
		return "", fmt.Errorf("nested workers are disabled")
	})
	defer child.Close()
	child.Facts, child.FactScope = a.Facts, a.FactScope
	if mem, err := memory.OpenScopedUser(a.Workspace.Root, childSession.ID, a.FactScope.User); err == nil {
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
func (a *Agent) RootDir() string { return a.Workspace.Root }
func (a *Agent) ReadSnapshot(ctx context.Context, path string) ([]byte, error) {
	return a.Workspace.ReadToolSnapshot(ctx, path, a.activePlanPath())
}
func (a *Agent) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return a.Workspace.ReadFile(ctx, path)
}
func (a *Agent) WriteFile(ctx context.Context, path string, data []byte) error {
	err := a.Workspace.WriteToolFile(ctx, path, data, a.activePlanPath())
	if err == nil {
		a.Memory.Invalidate(path)
	}
	return err
}
func (a *Agent) PatchFile(ctx context.Context, path, oldText, newText string) error {
	err := a.Workspace.PatchToolFile(ctx, path, oldText, newText, a.activePlanPath())
	if err == nil {
		a.Memory.Invalidate(path)
	}
	return err
}
func (a *Agent) activePlanPath() string {
	if active, path := a.Plan.Active(); active {
		return path
	}
	return ""
}
func (a *Agent) ShellEnvironment() []string { return workspace.ShellEnv() }
func (a *Agent) ExecuteShell(ctx context.Context, command string, seconds int) (string, error) {
	result, err := a.ExecuteShellResult(ctx, command, seconds)
	if err != nil {
		return "", err
	}
	return result.Output, nil
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
	data, err := a.Workspace.ReadToolSnapshot(ctx, p, a.activePlanPath())
	if err != nil {
		return "", err
	}
	if len(data) > 20<<20 {
		return "", fmt.Errorf("image exceeds the 20 MiB limit")
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
func (a *Agent) StartWorkerContext(ctx context.Context, role, prompt string) (string, error) {
	return a.Workers.StartContext(ctx, role, prompt)
}
func (a *Agent) StopWorkerContext(ctx context.Context, id string) error {
	return a.Workers.StopContext(ctx, id)
}
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
	// Runner bounds capture; executeTool persists the captured output before
	// truncating the model-visible result. Do not discard it at this layer.
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
