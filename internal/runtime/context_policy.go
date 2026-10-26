package runtime

import (
	"fmt"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/contextbuilder"
	"github.com/Lundomn/gopherharness/internal/provider"
)

func resolveContextPolicy(opts *Options) error {
	profile, _ := opts.Config.Selected()
	w, out, enc := profile.ContextPolicy()
	if p, ok := opts.Provider.(provider.ContextProfiler); ok {
		w, out, enc = p.ContextPolicy()
	}
	if opts.ContextWindowTokens != 0 {
		w = opts.ContextWindowTokens
	}
	if opts.MaxNewTokens == 0 {
		opts.MaxNewTokens = out
	}
	if opts.Tokenizer != "" {
		enc = opts.Tokenizer
	}
	if w <= 0 || opts.MaxNewTokens <= 0 || opts.MaxNewTokens >= w || opts.PromptBudget < 0 {
		return fmt.Errorf("invalid context window/output/byte limit")
	}
	if opts.HistorySelection != "" && opts.HistorySelection != "model" && opts.HistorySelection != "rule" {
		return fmt.Errorf("history selection must be model or rule")
	}
	counter := opts.TokenCounter
	if counter == nil {
		var err error
		counter, err = contextbuilder.NewCounter(enc)
		if err != nil {
			return err
		}
	}
	counters := contextbuilder.MaxCounter{counter}
	for _, c := range opts.FallbackClients {
		fw, fo, fe := (config.Provider{}).ContextPolicy()
		if p, ok := c.(provider.ContextProfiler); ok {
			fw, fo, fe = p.ContextPolicy()
		}
		if fw <= opts.MaxNewTokens || fo < opts.MaxNewTokens {
			return fmt.Errorf("fallback %s output/context budget too small", c.Model())
		}
		w = min(w, fw)
		fc, err := contextbuilder.NewCounter(fe)
		if err != nil {
			return err
		}
		counters = append(counters, fc)
	}
	if len(counters) > 1 {
		counter = counters
	}
	opts.ContextWindowTokens, opts.TokenCounter = w, counter
	return nil
}
