package provider

// WireUsage preserves field presence: missing values must not become known zero.
type WireUsage struct {
	Input      *int `json:"input_tokens"`
	Output     *int `json:"output_tokens"`
	CacheRead  *int `json:"cache_read_input_tokens"`
	CacheWrite *int `json:"cache_creation_input_tokens"`
	Details    *struct {
		Cached *int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (w WireUsage) Normalized(inputIncludesCache bool) Usage {
	u := Usage{}
	if w.Input != nil {
		u.InputTokens = *w.Input
	}
	if w.Output != nil {
		u.OutputTokens = *w.Output
	}
	if w.CacheRead != nil {
		u.CacheReadTokens = *w.CacheRead
	}
	if w.CacheWrite != nil {
		u.CacheWriteTokens = *w.CacheWrite
	}
	if w.Details != nil && w.Details.Cached != nil {
		u.CacheReadTokens = *w.Details.Cached
	}
	if inputIncludesCache {
		u.InputTokens -= u.CacheReadTokens + u.CacheWriteTokens
	}
	u.Complete = w.Input != nil && w.Output != nil && u.InputTokens >= 0 && u.OutputTokens >= 0 && u.CacheReadTokens >= 0 && u.CacheWriteTokens >= 0
	return u
}
