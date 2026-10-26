package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const DefaultProvider = "openai"

type Provider struct {
	Name                string
	Protocol            string
	APIKey              string
	BaseURL             string
	Model               string
	ContextWindowTokens int
	MaxOutputTokens     int
	Tokenizer           string
	// Explicit integer prices, nano-USD per million tokens. No live price guess.
	InputPrice, OutputPrice, CacheReadPrice, CacheWritePrice int64
	priceFields                                              uint8
}

func (p Provider) HasPrices() bool { return p.priceFields == 15 }

// ContextPolicy defaults are project policy, not discovery of a vendor's
// actual model limit. Profiles can override them for the selected model.
func (p Provider) ContextPolicy() (int, int, string) {
	w, o, enc := p.ContextWindowTokens, p.MaxOutputTokens, p.Tokenizer
	if w == 0 {
		w = 65536
	}
	if o == 0 {
		o = 8192
	}
	if enc == "" {
		enc = "cl100k_base"
	}
	return w, o, enc
}

type Sandbox struct {
	Mode           string
	Backend        string
	WorkspaceWrite bool
	DenyNetwork    bool
}

type Memory struct {
	BaseURL, APIKey string
	TimeoutSeconds  int
	ExtractFacts    bool
}

type Config struct {
	Provider  string
	Providers map[string]Provider
	Sandbox   Sandbox
	Runtime   RunPolicy
	Memory    Memory
}
type RunPolicy struct {
	ProviderRetries   int
	FallbackProviders []string
	MaxIterations     int
	CostLimitNanoUSD  int64
}

func Defaults() Config {
	return Config{
		Provider: DefaultProvider,
		Providers: map[string]Provider{
			"openai":    {Name: "openai", Protocol: "openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-5.4"},
			"anthropic": {Name: "anthropic", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", Model: "claude-sonnet-4-6"},
			"deepseek":  {Name: "deepseek", Protocol: "anthropic", BaseURL: "https://api.deepseek.com/anthropic", Model: "deepseek-v4-pro"},
		},
		Sandbox: Sandbox{Mode: "off", Backend: "auto", WorkspaceWrite: true},
		Runtime: RunPolicy{ProviderRetries: 3, MaxIterations: 40},
		Memory:  Memory{TimeoutSeconds: 30},
	}
}

func Load(start, explicit string) (Config, error) {
	cfg := Defaults()
	path := explicit
	if path == "" {
		path = findUp(start, ".pico.toml")
	}
	if path != "" {
		if err := parseFile(path, &cfg); err != nil {
			return Config{}, err
		}
	}
	applyEnv(&cfg)
	if _, ok := cfg.Providers[cfg.Provider]; !ok {
		return Config{}, fmt.Errorf("unknown provider profile %q", cfg.Provider)
	}
	return cfg, nil
}

func (c Config) Selected() (Provider, error) {
	p, ok := c.Providers[c.Provider]
	if !ok {
		return Provider{}, fmt.Errorf("unknown provider profile %q", c.Provider)
	}
	return p, nil
}

func parseFile(path string, cfg *Config) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	section := ""
	s := bufio.NewScanner(f)
	for lineNo := 1; s.Scan(); lineNo++ {
		line := strings.TrimSpace(stripComment(s.Text()))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: invalid TOML assignment", path, lineNo)
		}
		key, raw = strings.TrimSpace(key), strings.TrimSpace(raw)
		value, err := scalar(raw)
		if err != nil {
			return fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
		switch {
		case section == "" && key == "provider":
			cfg.Provider = value
		case strings.HasPrefix(section, "providers."):
			name := strings.TrimPrefix(section, "providers.")
			p := cfg.Providers[name]
			p.Name = name
			switch key {
			case "protocol":
				p.Protocol = value
			case "api_key":
				p.APIKey = value
			case "base_url":
				p.BaseURL = value
			case "model":
				p.Model = value
			case "context_window_tokens", "max_output_tokens":
				n, e := strconv.Atoi(value)
				if e != nil || n <= 0 {
					return fmt.Errorf("%s:%d: %s must be a positive integer", path, lineNo, key)
				}
				if key == "context_window_tokens" {
					p.ContextWindowTokens = n
				} else {
					p.MaxOutputTokens = n
				}
			case "tokenizer":
				p.Tokenizer = value
			case "input_price_nanousd_per_million", "output_price_nanousd_per_million", "cache_read_price_nanousd_per_million", "cache_write_price_nanousd_per_million":
				n, e := strconv.ParseInt(value, 10, 64)
				if e != nil || n < 0 {
					return fmt.Errorf("%s:%d: %s must be a nonnegative integer", path, lineNo, key)
				}
				switch key {
				case "input_price_nanousd_per_million":
					p.InputPrice = n
					p.priceFields |= 1
				case "output_price_nanousd_per_million":
					p.OutputPrice = n
					p.priceFields |= 2
				case "cache_read_price_nanousd_per_million":
					p.CacheReadPrice = n
					p.priceFields |= 4
				case "cache_write_price_nanousd_per_million":
					p.CacheWritePrice = n
					p.priceFields |= 8
				}
			}
			cfg.Providers[name] = p
		case section == "runtime":
			switch key {
			case "fallback_providers":
				cfg.Runtime.FallbackProviders = nil
				for _, name := range strings.Split(value, ",") {
					name = strings.TrimSpace(name)
					if name != "" {
						cfg.Runtime.FallbackProviders = append(cfg.Runtime.FallbackProviders, name)
					}
				}
			case "provider_retries", "max_iterations", "cost_limit_nanousd":
				n, e := strconv.ParseInt(value, 10, 64)
				if e != nil || n < 0 {
					return fmt.Errorf("%s:%d: invalid runtime value", path, lineNo)
				}
				switch key {
				case "provider_retries":
					if n > 3 {
						return fmt.Errorf("provider_retries must be in [0,3]")
					}
					cfg.Runtime.ProviderRetries = int(n)
				case "max_iterations":
					if n < 1 || n > 1000 {
						return fmt.Errorf("max_iterations must be in [1,1000]")
					}
					cfg.Runtime.MaxIterations = int(n)
				case "cost_limit_nanousd":
					cfg.Runtime.CostLimitNanoUSD = n
				}
			}
		case section == "memory":
			switch key {
			case "base_url":
				cfg.Memory.BaseURL = value
			case "api_key":
				cfg.Memory.APIKey = value
			case "timeout_seconds":
				n, e := strconv.Atoi(value)
				if e != nil || n < 1 || n > 300 {
					return fmt.Errorf("memory timeout_seconds must be in [1,300]")
				}
				cfg.Memory.TimeoutSeconds = n
			case "extract_facts":
				b, e := strconv.ParseBool(value)
				if e != nil {
					return fmt.Errorf("memory extract_facts must be boolean")
				}
				cfg.Memory.ExtractFacts = b
			}
		case section == "sandbox":
			switch key {
			case "mode":
				cfg.Sandbox.Mode = value
			case "backend":
				cfg.Sandbox.Backend = value
			case "workspace_write":
				b, e := strconv.ParseBool(value)
				if e != nil {
					return fmt.Errorf("sandbox workspace_write must be boolean")
				}
				cfg.Sandbox.WorkspaceWrite = b
			case "deny_network":
				b, e := strconv.ParseBool(value)
				if e != nil {
					return fmt.Errorf("sandbox deny_network must be boolean")
				}
				cfg.Sandbox.DenyNetwork = b
			}
		}
	}
	return s.Err()
}

func scalar(raw string) (string, error) {
	if raw == "true" || raw == "false" {
		return raw, nil
	}
	if _, err := strconv.Atoi(raw); err == nil {
		return raw, nil
	}
	v, err := strconv.Unquote(raw)
	if err != nil {
		return "", errors.New("only quoted strings, integers and booleans are supported")
	}
	return v, nil
}

func stripComment(line string) string {
	inQuote := false
	for i, r := range line {
		if r == '"' {
			inQuote = !inQuote
		}
		if r == '#' && !inQuote {
			return line[:i]
		}
	}
	return line
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("GOPHERHARNESS_MEMORY_URL"); v != "" {
		cfg.Memory.BaseURL = v
	}
	if v := os.Getenv("GOPHERHARNESS_MEMORY_API_KEY"); v != "" {
		cfg.Memory.APIKey = v
	}
	if v := os.Getenv("PICO_PROVIDER"); v != "" {
		cfg.Provider = strings.ToLower(v)
	}
	p := cfg.Providers[cfg.Provider]
	p.Name = cfg.Provider
	prefix := strings.ToUpper(cfg.Provider)
	if v := firstEnv("PICO_API_KEY", prefix+"_API_KEY", "PICO_"+prefix+"_API_KEY"); v != "" {
		p.APIKey = v
	}
	if v := firstEnv("PICO_BASE_URL", prefix+"_BASE_URL", "PICO_"+prefix+"_API_BASE"); v != "" {
		p.BaseURL = v
	}
	if v := firstEnv("PICO_MODEL", prefix+"_MODEL", "PICO_"+prefix+"_MODEL"); v != "" {
		p.Model = v
	}
	cfg.Providers[cfg.Provider] = p
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func findUp(start, name string) string {
	p, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(p, name)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
		parent := filepath.Dir(p)
		if parent == p {
			return ""
		}
		p = parent
	}
}
