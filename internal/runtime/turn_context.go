package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Lundomn/gopherharness/internal/contextbuilder"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/tools"
)

type turnContext struct {
	native    bool
	input     contextbuilder.Input
	messages  []model.Message
	prompt    string
	consumed  int
	meta      map[string]any
	catalog   *tools.Catalog
	finalOnly bool
}

func (a *Agent) prepareTurnContext(ctx context.Context, request string) (*turnContext, error) {
	mem, err := a.memoryInput(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("memory input: %w", err)
	}
	skills := a.Skills.ResolveWithReferences(request, len(a.allowedTools) == 0 || a.allowedTools["skill_read"])
	catalog := a.Tools.Snapshot()
	in := contextbuilder.Input{Workspace: a.Workspace.Summary(), ActiveSkills: skills.ActiveText, Skills: skills.SelectedText, Memory: mem.Text, History: a.Session.Messages, UserMessage: request, Tools: a.catalogSpecs(catalog), MaxChars: a.PromptBudget, MaxTokens: a.ContextWindowTokens, OutputReserve: a.MaxNewTokens, Counter: a.TokenCounter}
	if a.curatorEnabled {
		in.Curator = &contextbuilder.CuratorConfig{Complete: a.curatorComplete, SessionKey: a.Session.ID, ArchiveDir: filepath.Join(a.Workspace.Root, ".pico", "context-archives"), Recall: func(ctx context.Context, q string) (string, error) { m, e := a.memoryInput(ctx, q); return m.Text, e }}
		in.Curator.Redact = a.Redactor.Text
	}
	s := &turnContext{native: provider.HasNativeTools(a.Provider), input: in, consumed: len(a.Session.Messages), catalog: catalog}
	var meta contextbuilder.Metadata
	if s.native {
		s.messages, meta, err = contextbuilder.BuildMessagesChecked(ctx, in, a.HistorySelector)
	} else {
		s.prompt, meta, err = contextbuilder.BuildChecked(ctx, in, a.HistorySelector)
	}
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(meta)
	_ = json.Unmarshal(raw, &s.meta)
	s.meta["memory"], s.meta["skills"] = mem.Metadata, skills.Metadata
	return s, nil
}
func (a *Agent) curatorComplete(ctx context.Context, prompt string, n int) (string, error) {
	n = min(n, a.MaxNewTokens)
	count, err := a.TokenCounter.Count(prompt)
	if err != nil {
		return "", err
	}
	if count+8+n > a.ContextWindowTokens {
		return "", contextbuilder.ErrBudget
	}
	r, err := a.Provider.Complete(ctx, prompt, n)
	return r.Text, err
}
func (s *turnContext) advance(a *Agent) error {
	incoming := a.Session.Messages[s.consumed:]
	if s.native {
		s.messages = append(s.messages, incoming...)
	} else {
		for _, m := range incoming {
			b, e := json.Marshal(m)
			if e != nil {
				return e
			}
			s.prompt += "\n" + string(b)
		}
	}
	s.consumed = len(a.Session.Messages)
	// Freeze one directory for counting, advertisement and dispatch. A registry
	// refresh can publish the next request, never mutate this request's contract.
	s.catalog = a.Tools.Snapshot()
	specs := a.catalogSpecs(s.catalog)
	if s.finalOnly {
		specs = nil
	}
	if !s.native {
		updated, err := contextbuilder.RefreshLegacyTools(s.prompt, s.input.Workspace, s.input.Tools, specs)
		if err != nil {
			return err
		}
		s.prompt = updated
	}
	s.input.Tools = specs
	return s.measure()
}

func (a *Agent) catalogSpecs(catalog *tools.Catalog) []tools.Spec {
	var result []tools.Spec
	for _, spec := range catalog.Specs() {
		if a.toolAllowed(spec.Name) {
			result = append(result, spec)
		}
	}
	return result
}
func (s *turnContext) measure() error {
	if s.native {
		meta, err := contextbuilder.MeasureMessages(s.input, s.messages)
		if errors.Is(err, contextbuilder.ErrBudget) && s.shrink() {
			meta, err = contextbuilder.MeasureMessages(s.input, s.messages)
		}
		s.meta["prompt_tokens"], s.meta["prompt_chars"] = meta.PromptTokens, meta.PromptChars
		return err
	}
	n, err := s.input.Counter.Count(s.prompt)
	if err != nil {
		return err
	}
	s.meta["prompt_tokens"], s.meta["prompt_chars"] = n+8, len(s.prompt)
	if n+8 > s.input.MaxTokens-s.input.OutputReserve || (s.input.MaxChars > 0 && len(s.prompt) > s.input.MaxChars) {
		return contextbuilder.ErrBudget
	}
	return nil
}

// Shrinking affects only this request, never the source session ledger. IDs,
// calls and the newest three results remain intact. Tools are never re-run.
func (s *turnContext) shrink() bool {
	if !s.native {
		return false
	}
	const marker = "[Older tool output omitted for context budget; original remains in session ledger.]"
	seen, changed := 0, false
	for i := len(s.messages) - 1; i >= 0; i-- {
		if s.messages[i].Role != "tool" {
			continue
		}
		seen++
		if seen <= 3 || len(s.messages[i].Content) <= len(marker) || s.messages[i].Content == marker {
			continue
		}
		s.messages[i].Content = marker
		changed = true
	}
	return changed
}
func (s *turnContext) complete(ctx context.Context, a *Agent) (provider.Completion, error) {
	if a.OnModelStart != nil {
		a.OnModelStart()
	}
	if s.native {
		req := provider.ChatRequest{Messages: s.messages, Tools: s.input.Tools, MaxOutputTokens: a.MaxNewTokens}
		if a.OnDelta != nil {
			if c, ok := a.Provider.(provider.ChatStreamClient); ok {
				return c.ChatStream(ctx, req, a.OnDelta)
			}
		}
		return a.Provider.(provider.ChatClient).Chat(ctx, req)
	}
	if c, ok := a.Provider.(provider.StreamClient); ok && a.OnDelta != nil {
		return c.Stream(ctx, s.prompt, a.MaxNewTokens, a.OnDelta)
	}
	return a.Provider.Complete(ctx, s.prompt, a.MaxNewTokens)
}
func nativeParsed(c provider.Completion) (kind string, text string) {
	if len(c.ToolCalls) > 0 {
		return "tools", ""
	}
	if strings.TrimSpace(c.Text) == "" {
		return "retry", "The last response was empty. Return a final answer or use an available tool."
	}
	return "final", strings.TrimSpace(c.Text)
}
