package app

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/governance"
	"github.com/Lundomn/gopherharness/internal/maintenance"
	"github.com/Lundomn/gopherharness/internal/mcpbridge"
	"github.com/Lundomn/gopherharness/internal/permission"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/runtime"
	"github.com/Lundomn/gopherharness/internal/sandbox"
)

const Version = "0.1.0"
const ProductName = "GopherHarness"

type Options struct {
	ForceTUI       bool
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

func Run(args []string, opts Options) int {
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	fs := flag.NewFlagSet("pico", flag.ContinueOnError)
	fs.SetOutput(opts.Stderr)
	cwd := fs.String("cwd", ".", "workspace directory")
	repoRoot := fs.String("repo-root", "", "override repository root")
	configPath := fs.String("config", "", "path to .pico.toml")
	providerName := fs.String("provider", "", "provider profile")
	apiKey := fs.String("api-key", "", "provider API key override")
	modelName := fs.String("model", "", "model override")
	baseURL := fs.String("base-url", "", "provider base URL override")
	visionProviderName := fs.String("vision-provider", "", "provider profile used for image inspection")
	visionAPIKey := fs.String("vision-api-key", "", "vision provider API key override")
	visionModel := fs.String("vision-model", "", "vision model override")
	visionBaseURL := fs.String("vision-base-url", "", "vision provider base URL override")
	visionTimeout := fs.Int("vision-timeout", 120, "vision request timeout in seconds")
	resume := fs.String("resume", "", "session id or latest")
	approval := fs.String("approval", "ask", "ask, auto, or never")
	asyncApproval := fs.Bool("async-approval", false, "persist pending approval and release the running turn")
	workerWrites := fs.Bool("allow-worker-writes", false, "allow child agents to modify files; requires --approval auto")
	approveID := fs.String("approve", "", "approve and resume a pending action; requires --resume")
	denyID := fs.String("deny", "", "deny and resume a pending action; requires --resume")
	activeCandidate := fs.Bool("active-candidate", false, "use the explicitly activated and verified maintenance worktree")
	maxSteps := fs.Int("max-steps", 50, "maximum tool steps")
	maxIterations := fs.Int("max-iterations", 0, "maximum model iterations (profile default: 40)")
	providerRetries := fs.Int("provider-retries", -1, "transient retries per provider, 0..3 (-1 uses config)")
	fallbackProviders := fs.String("fallback-providers", "", "comma-separated fallback profile names")
	costLimit := fs.Int64("cost-limit-nanousd", -1, "shared persistent session budget, USD * 1e9 (0 disables; -1 uses config)")
	maxTokens := fs.Int("max-new-tokens", 0, "maximum output tokens (profile default: 8192)")
	promptBudget := fs.Int("prompt-budget", 0, "optional additional prompt byte ceiling (0 disables; token budget always enforced)")
	contextWindow := fs.Int("context-window-tokens", 0, "model window tokens (profile default: 65536)")
	contextTokenizer := fs.String("context-tokenizer", "", "cl100k_base (default) or o200k_base local estimator")
	historySelection := fs.String("history-selector", "model", "history scoring: model or rule")
	mcpConfig := fs.String("mcp-config", "", "JSON file containing trusted MCP server definitions")
	finalReadiness := fs.String("final-readiness", "warn", "final-answer gate: off, warn, soft, or strict")
	noAutoDream := fs.Bool("no-auto-dream", false, "disable background memory consolidation")
	dreamInterval := fs.Float64("dream-interval", 24, "minimum hours between automatic dream runs")
	dreamMinSessions := fs.Int("dream-min-sessions", 5, "minimum sessions before automatic dream")
	sandboxMode := fs.String("sandbox", "off", "off, best_effort, or required")
	sandboxBackend := fs.String("sandbox-backend", "auto", "auto, bubblewrap, sandbox-exec, or none")
	denyNetwork := fs.Bool("deny-network", false, "deny shell networking; requires OS sandbox")
	noPersonalSkills := fs.Bool("no-personal-skills", false, "load project skills only")
	memoryURL := fs.String("memory-url", "", "Memory JSON v1 backend URL")
	extractFacts := fs.Bool("extract-facts", false, "enable model-based extraction for local long-term memory")
	tui := fs.Bool("tui", opts.ForceTUI, "launch terminal UI")
	repl := fs.Bool("repl", false, "launch plain REPL")
	nonInteractive := fs.Bool("non-interactive", false, "disable interactive approval")
	version := fs.Bool("version", false, "print version")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "GopherHarness - local coding agent harness\n\nUsage:\n  gopherharness [options] [prompt]\n  gopherharness-tui [options]\n\nOptions:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *version {
		fmt.Fprintln(opts.Stdout, Version)
		return 0
	}
	root := *cwd
	if *repoRoot != "" {
		root = *repoRoot
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return fail(opts.Stderr, err)
	}
	if *activeCandidate {
		abs, err = maintenance.ResolveActiveRoot(context.Background(), abs)
		if err != nil {
			return fail(opts.Stderr, err)
		}
	}
	if (*approveID != "" && *denyID != "") || ((*approveID != "" || *denyID != "") && (*resume == "" || len(fs.Args()) > 0)) {
		return fail(opts.Stderr, fmt.Errorf("use either --approve or --deny with --resume and no new prompt"))
	}
	cfg, err := config.Load(abs, *configPath)
	if err != nil {
		return fail(opts.Stderr, fmt.Errorf("config: %w", err))
	}
	if *providerRetries < -1 || *providerRetries > 3 || *costLimit < -1 || *maxIterations < 0 || *maxIterations > 1000 {
		return fail(opts.Stderr, fmt.Errorf("invalid retry, budget or iteration limit"))
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "sandbox":
			cfg.Sandbox.Mode = *sandboxMode
		case "sandbox-backend":
			cfg.Sandbox.Backend = *sandboxBackend
		case "deny-network":
			cfg.Sandbox.DenyNetwork = *denyNetwork
		case "memory-url":
			cfg.Memory.BaseURL = *memoryURL
		case "extract-facts":
			cfg.Memory.ExtractFacts = *extractFacts
		}
	})
	if *providerName != "" {
		cfg.Provider = strings.ToLower(*providerName)
	}
	p, ok := cfg.Providers[cfg.Provider]
	if !ok {
		return fail(opts.Stderr, fmt.Errorf("unknown provider %q", cfg.Provider))
	}
	if *apiKey != "" {
		p.APIKey = *apiKey
	}
	if *modelName != "" {
		p.Model = *modelName
	}
	if *baseURL != "" {
		p.BaseURL = *baseURL
	}
	cfg.Providers[cfg.Provider] = p
	if *providerRetries >= 0 {
		cfg.Runtime.ProviderRetries = *providerRetries
	}
	if *costLimit >= 0 {
		cfg.Runtime.CostLimitNanoUSD = *costLimit
	}
	if *maxIterations != 0 {
		cfg.Runtime.MaxIterations = *maxIterations
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "fallback-providers" {
			cfg.Runtime.FallbackProviders = nil
			for _, name := range strings.Split(*fallbackProviders, ",") {
				if name = strings.TrimSpace(name); name != "" {
					cfg.Runtime.FallbackProviders = append(cfg.Runtime.FallbackProviders, name)
				}
			}
		}
	})
	client, err := provider.New(p, 300)
	if err != nil {
		return fail(opts.Stderr, err)
	}
	var vision provider.VisionClient
	if *visionProviderName != "" || *visionAPIKey != "" || *visionModel != "" || *visionBaseURL != "" {
		name := strings.ToLower(*visionProviderName)
		if name == "" {
			name = cfg.Provider
		}
		vp, exists := cfg.Providers[name]
		if !exists {
			return fail(opts.Stderr, fmt.Errorf("unknown vision provider %q", name))
		}
		if *visionAPIKey != "" {
			vp.APIKey = *visionAPIKey
		}
		if *visionModel != "" {
			vp.Model = *visionModel
		}
		if *visionBaseURL != "" {
			vp.BaseURL = *visionBaseURL
		}
		visionClient, createErr := provider.New(vp, *visionTimeout)
		if createErr != nil {
			return fail(opts.Stderr, createErr)
		}
		var supported bool
		vision, supported = visionClient.(provider.VisionClient)
		if !supported {
			return fail(opts.Stderr, fmt.Errorf("vision provider %q does not support image input", name))
		}
	}
	policy := permission.Policy(*approval)
	if policy != permission.Ask && policy != permission.Auto && policy != permission.Never {
		return fail(opts.Stderr, fmt.Errorf("invalid approval policy %q", *approval))
	}
	readinessMode, validReadiness := governance.ParseMode(*finalReadiness)
	if !validReadiness {
		return fail(opts.Stderr, fmt.Errorf("invalid final readiness mode %q", *finalReadiness))
	}
	var approver permission.Approver
	if !*nonInteractive && policy == permission.Ask {
		approver = permission.TerminalApprover{In: opts.Stdin, Out: opts.Stdout}
	}
	agentOptions := runtime.Options{
		Root:                  abs,
		Config:                cfg,
		Provider:              client,
		Vision:                vision,
		Resume:                *resume,
		Approval:              policy,
		Approver:              approver,
		AsyncApproval:         *asyncApproval,
		AllowWorkerWrites:     *workerWrites,
		MaxSteps:              *maxSteps,
		MaxAttempts:           cfg.Runtime.MaxIterations,
		MaxNewTokens:          *maxTokens,
		PromptBudget:          *promptBudget,
		ContextWindowTokens:   *contextWindow,
		Tokenizer:             *contextTokenizer,
		HistorySelection:      *historySelection,
		FinalReadiness:        readinessMode,
		Sandbox:               sandbox.Config{Mode: cfg.Sandbox.Mode, Backend: cfg.Sandbox.Backend, WorkspaceWrite: cfg.Sandbox.WorkspaceWrite, DenyNetwork: cfg.Sandbox.DenyNetwork},
		DisablePersonalSkills: *noPersonalSkills,
		DisableAutoDream:      *noAutoDream,
		DreamInterval:         time.Duration(*dreamInterval * float64(time.Hour)),
		DreamMinSessions:      *dreamMinSessions,
		Secrets:               []string{p.APIKey, *visionAPIKey},
		Input:                 opts.Stdin,
		Output:                opts.Stdout,
	}
	if *mcpConfig != "" {
		data, readErr := os.ReadFile(*mcpConfig)
		if readErr != nil {
			return fail(opts.Stderr, readErr)
		}
		var servers []mcpbridge.Server
		if err := json.Unmarshal(data, &servers); err != nil {
			return fail(opts.Stderr, fmt.Errorf("MCP config: %w", err))
		}
		agentOptions.MCPServers = servers
	}
	agent, err := runtime.New(agentOptions)
	if err != nil {
		return fail(opts.Stderr, err)
	}
	defer agent.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *approveID != "" || *denyID != "" {
		id, allow := *approveID, true
		if id == "" {
			id, allow = *denyID, false
		}
		answer, err := agent.ResumeApproval(ctx, id, agent.FactScope.User, allow)
		if err != nil {
			return fail(opts.Stderr, err)
		}
		fmt.Fprintln(opts.Stdout, answer)
		return 0
	}
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt != "" {
		answer, err := agent.Ask(ctx, prompt)
		if err != nil {
			return fail(opts.Stderr, err)
		}
		fmt.Fprintln(opts.Stdout, answer)
		return 0
	}
	if *tui && !*repl {
		return runTUI(ctx, agent, opts)
	}
	return runREPL(ctx, agent, opts, false)
}

func runREPL(ctx context.Context, agent *runtime.Agent, opts Options, tui bool) int {
	var streamOutput *finalStreamOutput
	if tui {
		streamOutput = &finalStreamOutput{out: opts.Stdout, native: provider.HasNativeTools(agent.Provider)}
		agent.OnModelStart = streamOutput.Start
		agent.OnDelta = streamOutput.Delta
	}
	if tui {
		drawHeader(opts.Stdout, agent)
	} else {
		fmt.Fprintf(opts.Stdout, "%s %s | session %s | /help for commands\n", ProductName, Version, agent.SessionID())
	}
	scanner := bufio.NewScanner(opts.Stdin)
	for {
		if tui {
			fmt.Fprint(opts.Stdout, "\n\033[36mgopher›\033[0m ")
		} else {
			fmt.Fprint(opts.Stdout, "\ngopherharness> ")
		}
		if !scanner.Scan() {
			fmt.Fprintln(opts.Stdout)
			return 0
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			handled, exit := slash(ctx, agent, line, opts.Stdout)
			if handled {
				if exit {
					return 0
				}
				continue
			}
		}
		if tui {
			fmt.Fprintln(opts.Stdout, "\033[2mworking...\033[0m")
		}
		answer, err := agent.Ask(ctx, line)
		if err != nil {
			fmt.Fprintf(opts.Stderr, "error: %v\n", err)
			continue
		}
		if tui {
			if !streamOutput.Finish(answer) {
				fmt.Fprintf(opts.Stdout, "\n\033[1;32massistant\033[0m\n%s\n", answer)
			}
		} else {
			fmt.Fprintln(opts.Stdout, answer)
		}
	}
}
func runTUI(ctx context.Context, agent *runtime.Agent, opts Options) int {
	return runREPL(ctx, agent, opts, true)
}
func drawHeader(w io.Writer, agent *runtime.Agent) {
	fmt.Fprint(w, "\033[2J\033[H")
	fmt.Fprintln(w, "╭──────────────────────────────────────────────────────────────────────╮")
	fmt.Fprintf(w, "│ %s %-54s│\n", ProductName, Version)
	fmt.Fprintf(w, "│ session: %-60s│\n", agent.SessionID())
	fmt.Fprintln(w, "│ Local coding agent · runtime · tools · memory · evidence             │")
	fmt.Fprintln(w, "╰──────────────────────────────────────────────────────────────────────╯")
}

func slash(ctx context.Context, agent *runtime.Agent, line string, out io.Writer) (bool, bool) {
	parts := strings.Fields(line)
	cmd := parts[0]
	switch cmd {
	case "/help":
		fmt.Fprintln(out, "/help /session /memory /dream /skills /mcp refresh NAME /mcp reconnect NAME [1..3] /todo /workers /usage /budget /approvals /approve ID /deny ID /reset /exit")
		return true, false
	case "/session":
		fmt.Fprintf(out, "session: %s\nlatest run: %s\n", agent.SessionID(), agent.LastRunID())
		return true, false
	case "/mcp":
		if agent.MCP == nil {
			fmt.Fprintln(out, "未配置 MCP 服务。")
			return true, false
		}
		if len(parts) < 3 || len(parts) > 4 || (parts[1] != "refresh" && parts[1] != "reconnect") {
			fmt.Fprintln(out, "用法：/mcp refresh 服务名 或 /mcp reconnect 服务名 [1..3]")
			return true, false
		}
		var err error
		if parts[1] == "refresh" {
			if len(parts) != 3 {
				fmt.Fprintln(out, "refresh 不接受次数参数")
				return true, false
			}
			err = agent.MCP.Refresh(ctx, parts[2])
		} else {
			attempts := 1
			if len(parts) == 4 {
				attempts, err = strconv.Atoi(parts[3])
			}
			if err == nil {
				err = agent.MCP.Reconnect(ctx, parts[2], attempts)
			}
		}
		if err != nil {
			fmt.Fprintln(out, err)
		} else {
			fmt.Fprintln(out, "工具目录已更新；不会重放旧调用，也不会扩大允许名单。")
		}
		return true, false
	case "/memory":
		fmt.Fprintln(out, agent.Memory.Render(""))
		return true, false
	case "/dream":
		result, err := agent.RunDream(ctx)
		if err != nil {
			fmt.Fprintf(out, "dream failed: %v\n", err)
		} else {
			fmt.Fprintln(out, result)
		}
		return true, false
	case "/skills":
		for _, skill := range agent.Skills.List() {
			fmt.Fprintf(out, "- %s: %s (%s)\n", skill.Name, skill.Description, skill.Path)
		}
		return true, false
	case "/todo":
		fmt.Fprintln(out, agent.TodoList())
		return true, false
	case "/workers":
		for _, w := range agent.Workers.List() {
			fmt.Fprintf(out, "- %s [%s] role=%s result=%s error=%s\n", w.ID, w.Status, w.Role, clip(w.Result, 120), w.Error)
		}
		return true, false
	case "/usage":
		in, outTokens, calls := agent.Usage()
		fmt.Fprintf(out, "model_calls=%d input_tokens=%d output_tokens=%d\n", calls, in, outTokens)
		return true, false
	case "/reset":
		if err := agent.Reset(); err != nil {
			fmt.Fprintf(out, "reset failed: %v\n", err)
		} else {
			fmt.Fprintf(out, "new session: %s\n", agent.SessionID())
		}
		return true, false
	case "/budget":
		if agent.CostBudget == nil {
			fmt.Fprintln(out, "未启用费用预算；账本仍记录已知用量。")
		} else {
			if len(parts) > 1 {
				if len(parts) < 5 || parts[1] != "reconcile" {
					fmt.Fprintln(out, "用法：/budget reconcile 尝试ID 实际纳美元费用 对账说明")
					return true, false
				}
				cost, err := strconv.ParseInt(parts[3], 10, 64)
				if err == nil {
					err = agent.CostBudget.Reconcile(ctx, parts[2], cost, strings.Join(parts[4:], " "))
				}
				if err != nil {
					fmt.Fprintln(out, err)
					return true, false
				}
			}
			snapshot, err := agent.CostBudget.Snapshot(ctx)
			if err != nil {
				fmt.Fprintln(out, err)
			} else {
				fmt.Fprintf(out, "预算=%d 已结算=%d 未知/在途占用=%d 剩余=%d 纳美元；冻结=%t\n", snapshot.LimitNanoUSD, snapshot.SpentNanoUSD, snapshot.HeldNanoUSD, snapshot.RemainingNanoUSD, snapshot.Frozen)
			}
		}
		return true, false
	case "/approvals":
		pending, err := agent.PendingApprovals(ctx)
		if err != nil {
			fmt.Fprintln(out, err)
		} else {
			b, _ := json.MarshalIndent(pending, "", "  ")
			fmt.Fprintln(out, string(b))
		}
		return true, false
	case "/approve", "/deny":
		if len(parts) != 2 {
			fmt.Fprintln(out, "用法：/approve 审批ID 或 /deny 审批ID")
			return true, false
		}
		answer, err := agent.ResumeApproval(ctx, parts[1], agent.FactScope.User, cmd == "/approve")
		if err != nil {
			fmt.Fprintln(out, err)
		} else {
			fmt.Fprintln(out, answer)
		}
		return true, false
	case "/exit", "/quit":
		return true, true
	case "/maxsteps":
		if len(parts) == 2 {
			if n, err := strconv.Atoi(parts[1]); err == nil && n > 0 {
				agent.MaxSteps = n
				fmt.Fprintf(out, "max steps: %d\n", n)
				return true, false
			}
		}
		fmt.Fprintln(out, "usage: /maxsteps N")
		return true, false
	default:
		return false, false
	}
}
func fail(w io.Writer, err error) int { fmt.Fprintf(w, "pico: %v\n", err); return 1 }
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type finalStreamOutput struct {
	out     io.Writer
	native  bool
	buffer  string
	emitted int
	printed bool
}

func (s *finalStreamOutput) Start() {
	s.buffer = ""
	s.emitted = 0
	s.printed = false
}

func (s *finalStreamOutput) Delta(delta string) error {
	s.buffer += delta
	if s.native && strings.HasPrefix("<final>", s.buffer) {
		return nil
	}
	if s.native && !strings.HasPrefix(s.buffer, "<final>") {
		if s.emitted == len(s.buffer) {
			return nil
		}
		if !s.printed {
			if _, err := fmt.Fprint(s.out, "\n\033[1;32massistant\033[0m\n"); err != nil {
				return err
			}
			s.printed = true
		}
		n, err := io.WriteString(s.out, s.buffer[s.emitted:])
		s.emitted += n
		return err
	}
	start := strings.Index(s.buffer, "<final>")
	if start < 0 {
		return nil
	}
	content := s.buffer[start+len("<final>"):]
	end := strings.Index(content, "</final>")
	safe := len(content) - len("</final>")
	if end >= 0 {
		safe = end
	}
	if safe < 0 {
		safe = 0
	}
	if safe <= s.emitted {
		return nil
	}
	if !s.printed {
		fmt.Fprint(s.out, "\n\033[1;32massistant\033[0m\n")
		s.printed = true
	}
	_, err := fmt.Fprint(s.out, content[s.emitted:safe])
	s.emitted = safe
	return err
}

func (s *finalStreamOutput) Finish(answer string) bool {
	if !s.printed {
		return false
	}
	if s.emitted < len(answer) {
		fmt.Fprint(s.out, answer[s.emitted:])
	}
	fmt.Fprintln(s.out)
	return true
}
