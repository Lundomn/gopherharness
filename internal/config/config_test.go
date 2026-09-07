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
	if cfg.Provider != "deepseek" || p.APIKey != "env-key" || p.Protocol != "anthropic" {
		t.Fatalf("unexpected config: %#v %#v", cfg, p)
	}
}
