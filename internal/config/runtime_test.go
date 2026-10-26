package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeMemoryAndPricesConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	data := `[runtime]
provider_retries=2
max_iterations=40
cost_limit_nanousd=1000000000
fallback_providers="anthropic"
[memory]
base_url="https://memory.example.test"
api_key="test-key"
timeout_seconds=25
extract_facts=true
[sandbox]
mode="required"
deny_network=true
[providers.openai]
input_price_nanousd_per_million=1000000000
output_price_nanousd_per_million=2000000000
cache_read_price_nanousd_per_million=0
cache_write_price_nanousd_per_million=0
`
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	c := Defaults()
	if e := parseFile(p, &c); e != nil {
		t.Fatal(e)
	}
	if c.Runtime.ProviderRetries != 2 || len(c.Runtime.FallbackProviders) != 1 || !c.Providers["openai"].HasPrices() || !c.Memory.ExtractFacts || c.Memory.TimeoutSeconds != 25 || !c.Sandbox.DenyNetwork {
		t.Fatalf("%+v", c)
	}
	for _, s := range []string{"[runtime]\nprovider_retries=4", "[runtime]\nmax_iterations=0", "[memory]\ntimeout_seconds=0", "[memory]\nextract_facts=abc", "[sandbox]\ndeny_network=2", "[providers.openai]\ninput_price_nanousd_per_million=-1"} {
		if e := os.WriteFile(p, []byte(s), 0600); e != nil {
			t.Fatal(e)
		}
		c = Defaults()
		if e := parseFile(p, &c); e == nil {
			t.Fatal("accepted", s)
		}
	}
}
