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
	Name     string
	Protocol string
	APIKey   string
	BaseURL  string
	Model    string
}

type Sandbox struct {
	Mode           string
	Backend        string
	WorkspaceWrite bool
}

type Config struct {
	Provider  string
	Providers map[string]Provider
	Sandbox   Sandbox
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
			}
			cfg.Providers[name] = p
		case section == "sandbox":
			switch key {
			case "mode":
				cfg.Sandbox.Mode = value
			case "backend":
				cfg.Sandbox.Backend = value
			case "workspace_write":
				cfg.Sandbox.WorkspaceWrite = value == "true"
			}
		}
	}
	return s.Err()
}

func scalar(raw string) (string, error) {
	if raw == "true" || raw == "false" {
		return raw, nil
	}
	v, err := strconv.Unquote(raw)
	if err != nil {
		return "", errors.New("only quoted strings and booleans are supported")
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
