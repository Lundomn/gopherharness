package sandbox

import "sync"

// MaxOutputBytes bounds stdout and stderr together. Each stream retains its
// beginning and end; writers are always drained to avoid blocking the child.
const MaxOutputBytes = 1 << 20

type boundedOutput struct {
	mu         sync.Mutex
	limit      int
	head, tail []byte
	seen       int64
}

func newBoundedOutput(limit int) *boundedOutput { return &boundedOutput{limit: limit} }

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.seen += int64(n)
	half := b.limit / 2
	if len(b.head) < half {
		take := min(half-len(b.head), len(p))
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	tailLimit := b.limit - half
	if len(p) >= tailLimit {
		b.tail = append(b.tail[:0], p[len(p)-tailLimit:]...)
	} else if len(b.tail)+len(p) > tailLimit {
		remove := len(b.tail) + len(p) - tailLimit
		copy(b.tail, b.tail[remove:])
		b.tail = b.tail[:len(b.tail)-remove]
		b.tail = append(b.tail, p...)
	} else {
		b.tail = append(b.tail, p...)
	}
	return n, nil
}

func (b *boundedOutput) snapshot() (string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen <= int64(b.limit) {
		return string(b.head) + string(b.tail), 0
	}
	const marker = "\n[output truncated: middle omitted; exit status is unchanged]\n"
	// Reserve the marker inside the limit, rather than silently exceeding it.
	headSize := (b.limit - len(marker)) / 2
	tailSize := b.limit - len(marker) - headSize
	return string(b.head[:headSize]) + marker + string(b.tail[len(b.tail)-tailSize:]), b.seen - int64(headSize+tailSize)
}
