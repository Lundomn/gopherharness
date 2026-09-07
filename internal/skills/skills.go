package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Skill struct{ Name, Description, Path, Body string }
type Registry struct{ Skills map[string]Skill }

func Discover(root string) *Registry {
	r := &Registry{Skills: map[string]Skill{}}
	dirs := []string{filepath.Join(root, ".pico", "skills"), filepath.Join(root, ".agents", "skills")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".config", "pico", "skills"))
	}
	for _, base := range dirs {
		entries, _ := os.ReadDir(base)
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(base, entry.Name(), "SKILL.md")
			b, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			skill := parse(path, string(b), entry.Name())
			r.Skills[skill.Name] = skill
		}
	}
	return r
}
func (r *Registry) List() []Skill {
	out := make([]Skill, 0, len(r.Skills))
	for _, s := range r.Skills {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (r *Registry) Get(name string) (Skill, bool) { s, ok := r.Skills[name]; return s, ok }
func (r *Registry) Render(query string) string {
	list := r.List()
	if len(list) == 0 {
		return "Skills:\n- none"
	}
	lower := strings.ToLower(query)
	var b strings.Builder
	b.WriteString("Skills catalog:\n")
	for _, s := range list {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	for _, s := range list {
		if strings.Contains(lower, strings.ToLower(s.Name)) || overlap(lower, strings.ToLower(s.Description)) >= 2 {
			fmt.Fprintf(&b, "\nSelected skill %s (%s):\n%s\n", s.Name, s.Path, s.Body)
		}
	}
	text := b.String()
	if len(text) > 16000 {
		text = text[:16000] + "\n...[skills clipped]"
	}
	return strings.TrimSpace(text)
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
func overlap(a, b string) int {
	set := map[string]bool{}
	for _, v := range strings.Fields(b) {
		if len(v) > 3 {
			set[v] = true
		}
	}
	n := 0
	for _, v := range strings.Fields(a) {
		if set[v] {
			n++
		}
	}
	return n
}
