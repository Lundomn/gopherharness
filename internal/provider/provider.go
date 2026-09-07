package provider

import (
	"context"
	"fmt"
	"github.com/Lundomn/gopherharness/internal/config"
	"sync"
)

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type Completion struct {
	Text  string `json:"text"`
	Usage Usage  `json:"usage"`
}

type Client interface {
	Complete(context.Context, string, int) (Completion, error)
	Name() string
	Model() string
}

// VisionClient is implemented by providers that accept inline image input.
type VisionClient interface {
	InspectImage(context.Context, string, []byte, string, int) (Completion, error)
}

type StreamClient interface {
	Stream(context.Context, string, int, func(string) error) (Completion, error)
}

func New(p config.Provider, timeoutSeconds int) (Client, error) {
	switch p.Protocol {
	case "openai":
		return NewOpenAI(p, timeoutSeconds), nil
	case "anthropic":
		return NewAnthropic(p, timeoutSeconds), nil
	default:
		return nil, fmt.Errorf("unsupported provider protocol %q", p.Protocol)
	}
}

type Fake struct {
	ProviderName string
	ModelName    string
	Responses    []string
	Index        int
	mu           sync.Mutex
}

func (f *Fake) Complete(_ context.Context, _ string, _ int) (Completion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Index >= len(f.Responses) {
		return Completion{}, fmt.Errorf("fake responses exhausted")
	}
	r := f.Responses[f.Index]
	f.Index++
	return Completion{Text: r}, nil
}
func (f *Fake) Name() string {
	if f.ProviderName == "" {
		return "fake"
	}
	return f.ProviderName
}
func (f *Fake) Model() string {
	if f.ModelName == "" {
		return "fake-model"
	}
	return f.ModelName
}
