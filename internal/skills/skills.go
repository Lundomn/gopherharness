package skills

import (
	"sort"
	"strings"
	"sync"
	"time"
)

type Skill struct {
	Name, Description, Path, Body string
	Always                        bool
}
type Registry struct {
	Skills      map[string]Skill // compatibility input; use Replace/RebuildIndex before concurrent use
	mu          sync.RWMutex
	index       *snapshot
	sources     []string
	refreshMu   sync.Mutex
	lastRefresh time.Time
	refreshErr  error
}

func Discover(root string) *Registry {
	return DiscoverScoped(root, true)
}
func (r *Registry) List() []Skill {
	r.refreshIfDue()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Skill, 0, len(r.Skills))
	for _, s := range r.Skills {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (r *Registry) Get(name string) (Skill, bool) {
	r.refreshIfDue()
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.Skills[name]
	return s, ok
}
func (r *Registry) Render(query string) string {
	s := r.Resolve(query)
	return strings.TrimSpace(s.ActiveText + "\n\n" + s.SelectedText)
}
func parse(path, raw, fallback string) Skill {
	s := Skill{Name: fallback, Path: path, Body: raw}
	if strings.HasPrefix(raw, "---\n") {
		if end := strings.Index(raw[4:], "\n---"); end >= 0 {
			header := raw[4 : 4+end]
			s.Body = strings.TrimSpace(raw[4+end+4:])
			for _, line := range strings.Split(header, "\n") {
				k, v, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				switch strings.TrimSpace(k) {
				case "name":
					s.Name = strings.Trim(strings.TrimSpace(v), `"'`)
				case "description":
					s.Description = strings.Trim(strings.TrimSpace(v), `"'`)
				case "always":
					s.Always = strings.TrimSpace(v) == "true"
				}
			}
		}
	}
	if s.Description == "" {
		for _, line := range strings.Split(s.Body, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				s.Description = line
				break
			}
		}
	}
	return s
}
