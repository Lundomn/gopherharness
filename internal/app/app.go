package app

import (
	"bufio"
	"context"
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
	maxSteps := fs.Int("max-steps", 50, "maximum tool steps")
	maxTokens := fs.Int("max-new-tokens", 8192, "maximum model output tokens")
	promptBudget := fs.Int("prompt-budget", 60000, "maximum prompt characters")
	finalReadiness := fs.String("final-readiness", "warn", "final-answer gate: off, warn, soft, or strict")
	noAutoDream := fs.Bool("no-auto-dream", false, "disable background memory consolidation")
	dreamInterval := fs.Float64("dream-interval", 24, "minimum hours between automatic dream runs")
	dreamMinSessions := fs.Int("dream-min-sessions", 5, "minimum sessions before automatic dream")
	sandboxMode := fs.String("sandbox", "off", "off, best_effort, or required")
	sandboxBackend := fs.String("sandbox-backend", "auto", "auto, bubblewrap, sandbox-exec, or none")
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
	cfg, err := config.Load(abs, *configPath)
	if err != nil {
		return fail(opts.Stderr, fmt.Errorf("config: %w", err))
	}
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
		Root:             abs,
		Config:           cfg,
		Provider:         client,
		Vision:           vision,
		Resume:           *resume,
		Approval:         policy,
		Approver:         approver,
		MaxSteps:         *maxSteps,
		MaxNewTokens:     *maxTokens,
		PromptBudget:     *promptBudget,
		FinalReadiness:   readinessMode,
		Sandbox:          sandbox.Config{Mode: *sandboxMode, Backend: *sandboxBackend, WorkspaceWrite: cfg.Sandbox.WorkspaceWrite},
		DisableAutoDream: *noAutoDream,
		DreamInterval:    time.Duration(*dreamInterval * float64(time.Hour)),
		DreamMinSessions: *dreamMinSessions,
		Secrets:          []string{p.APIKey, *visionAPIKey},
		Input:            opts.Stdin,
		Output:           opts.Stdout,
	}
	agent, err := runtime.New(agentOptions)
	if err != nil {
		return fail(opts.Stderr, err)
	}
	defer agent.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
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
		streamOutput = &finalStreamOutput{out: opts.Stdout}
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
		fmt.Fprintln(out, "/help /session /memory /dream /skills /todo /workers /usage /reset /exit")
		return true, false
	case "/session":
		fmt.Fprintf(out, "session: %s\nlatest run: %s\n", agent.SessionID(), agent.LastRunID())
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
