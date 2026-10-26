package contextbuilder

import (
	"fmt"
	"sync"

	"github.com/tiktoken-go/tokenizer"
)

const DefaultContextWindow = 65536
const DefaultOutputReserve = 8192
const DefaultFastThreshold = 0.60

// Counter counts the rendered text actually sent to the provider. Local BPE
// counts are estimates of provider usage, not a vendor billing guarantee.
// Implementations must be safe for concurrent calls (workers share a counter).
type Counter interface {
	Count(string) (int, error)
	Name() string
}

type localCounter struct {
	codec tokenizer.Codec
	mu    sync.Mutex
}

func (c *localCounter) Count(s string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.codec.Count(s)
}
func (c *localCounter) Name() string { return c.codec.GetName() + "_estimate" }

var counters sync.Map

// NewCounter uses embedded vocabularies; counting never calls a model or
// downloads data at runtime. cl100k_base matches the reference estimator.
func NewCounter(encoding string) (Counter, error) {
	if encoding == "" {
		encoding = "cl100k_base"
	}
	if v, ok := counters.Load(encoding); ok {
		return v.(Counter), nil
	}
	if encoding != "cl100k_base" && encoding != "o200k_base" {
		return nil, fmt.Errorf("unsupported context tokenizer %q", encoding)
	}
	codec, err := tokenizer.Get(tokenizer.Encoding(encoding))
	if err != nil {
		return nil, err
	}
	c, _ := counters.LoadOrStore(encoding, &localCounter{codec: codec})
	return c.(Counter), nil
}

// MaxCounter makes a single prompt safe under all configured local estimators
// before a retry wrapper can switch models. It never silently assumes that a
// fallback model has the same tokenizer as the primary.
type MaxCounter []Counter

func (cs MaxCounter) Count(s string) (int, error) {
	n := 0
	for _, c := range cs {
		v, err := c.Count(s)
		if err != nil {
			return 0, err
		}
		n = max(n, v)
	}
	return n, nil
}
func (cs MaxCounter) Name() string {
	s := "max("
	for i, c := range cs {
		if i > 0 {
			s += ","
		}
		s += c.Name()
	}
	return s + ")"
}
