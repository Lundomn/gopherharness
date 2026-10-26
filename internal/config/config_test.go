package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProjectConfigAndEnvironmentOverride(t *testing.T) {
	dir := t.TempDir()
	raw := `provider = "deepseek"
[providers.deepseek]
protocol = "anthropic"
api_key = "file-key"
base_url = "https://example.test/anthropic"
model = "deepseek-test"
context_window_tokens = 65536
max_output_tokens = 8192
tokenizer = "cl100k_base"
`
	if err := os.WriteFile(filepath.Join(dir, ".pico.toml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "env-key")
	cfg, err := Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := cfg.Selected()
	if w, o, enc := p.ContextPolicy(); w != 65536 || o != 8192 || enc != "cl100k_base" {
		t.Fatal("context profile not loaded")
	}
	if cfg.Provider != "deepseek" || p.APIKey != "env-key" || p.Protocol != "anthropic" {
		t.Fatalf("unexpected config: %#v %#v", cfg, p)
	}
}

func TestRejectInvalidContextNumbers(t *testing.T) {
	for _, value := range []string{"0", "-1", "true", "\"bad\""} {
		file := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(file, []byte("[providers.openai]\ncontext_window_tokens = "+value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		c := Defaults()
		if err := parseFile(file, &c); err == nil {
			t.Fatal("invalid limit accepted", value)
		}
	}
}
