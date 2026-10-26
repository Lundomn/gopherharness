package runtime

import (
	"fmt"

	"github.com/Lundomn/gopherharness/internal/provider"
)

func applyProviderConfig(opts *Options) error {
	if opts.ProviderRetries == 0 {
		opts.ProviderRetries = opts.Config.Runtime.ProviderRetries
	}
	if opts.ProviderRetries < 0 || opts.ProviderRetries > 3 {
		return fmt.Errorf("provider retries must be in [0,3]")
	}
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = opts.Config.Runtime.MaxIterations
	}
	if opts.MaxAttempts < 0 || opts.MaxAttempts > 1000 {
		return fmt.Errorf("maximum iterations must be in [1,1000]")
	}
	if opts.CostLimitNanoUSD == 0 {
		opts.CostLimitNanoUSD = opts.Config.Runtime.CostLimitNanoUSD
	}
	if opts.CostLimitNanoUSD < 0 {
		return fmt.Errorf("cost limit must not be negative")
	}
	if len(opts.FallbackClients) == 0 {
		seen := map[string]bool{opts.Config.Provider: true}
		for _, name := range opts.Config.Runtime.FallbackProviders {
			if seen[name] {
				return fmt.Errorf("duplicate fallback profile %q", name)
			}
			seen[name] = true
			p, ok := opts.Config.Providers[name]
			if !ok {
				return fmt.Errorf("unknown fallback profile %q", name)
			}
			c, e := provider.New(p, 300)
			if e != nil {
				return e
			}
			opts.FallbackClients = append(opts.FallbackClients, c)
		}
	}
	prices := map[string]provider.Price{}
	for _, p := range opts.Config.Providers {
		if p.HasPrices() {
			prices[p.Model] = provider.Price{Input: float64(p.InputPrice) / 1e9, Output: float64(p.OutputPrice) / 1e9, CacheRead: float64(p.CacheReadPrice) / 1e9, CacheWrite: float64(p.CacheWritePrice) / 1e9}
		}
	}
	for k, p := range opts.Prices {
		prices[k] = p
	}
	opts.Prices = prices
	return nil
}
