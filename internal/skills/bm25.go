package skills

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const BM25K1 = 1.5
const BM25B = 0.75
const CandidateLimit = 5
const ActivationLimit = 2
const AlwaysLimit = 5
const RRFConstant = 60

var termRE = regexp.MustCompile(`[a-z0-9]{2,}|[一-鿿]`)
var nameWordsRE = regexp.MustCompile(`[a-z0-9]{2,}`)
var nameSeparators = regexp.MustCompile(`[-_/]+`)
var stopWords = map[string]bool{"and": true, "for": true, "from": true, "into": true, "that": true, "the": true, "this": true, "use": true, "using": true, "with": true, "your": true}

func Tokenize(text string) []string { return termRE.FindAllString(strings.ToLower(text), -1) }

type snapshot struct {
	skills      []Skill
	frequencies []map[string]int
	lengths     []int
	idf         map[string]float64
	average     float64
}
type Hit struct {
	Skill    Skill
	Score    float64
	RRFScore float64
}
type SelectionMetadata struct {
	CandidateIDs          []string `json:"candidate_skill_ids"`
	InjectedIDs           []string `json:"injected_skill_ids"`
	ReferencedIDs         []string `json:"referenced_skill_ids"`
	AlwaysIDs             []string `json:"always_skill_ids"`
	UnavailableReferences []string `json:"unavailable_reference_ids,omitempty"`
	Method                string   `json:"method"`
}
type Resolution struct {
	ActiveText, SelectedText string
	Metadata                 SelectionMetadata
}

func firstRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
func buildSnapshot(list []Skill) *snapshot {
	s := &snapshot{skills: append([]Skill(nil), list...), idf: map[string]float64{}}
	df := map[string]int{}
	for _, skill := range list {
		terms := Tokenize(skill.Name + " " + skill.Name + " " + skill.Description + " " + firstRunes(skill.Body, 4000))
		f := map[string]int{}
		for _, term := range terms {
			f[term]++
		}
		for term := range f {
			df[term]++
		}
		s.frequencies = append(s.frequencies, f)
		s.lengths = append(s.lengths, len(terms))
		s.average += float64(len(terms))
	}
	if len(list) > 0 {
		s.average /= float64(len(list))
	}
	for term, n := range df {
		s.idf[term] = math.Log1p((float64(len(list)-n) + .5) / (float64(n) + .5))
	}
	return s
}

// Rebuild creates the expensive index outside the lock and atomically swaps the
// paired skill/index snapshot. In-flight queries retain the complete old pair.
// Replace is the safe update entry point; direct mutation of Skills is not safe.
func (r *Registry) RebuildIndex() {
	s := buildSnapshot(r.List())
	r.mu.Lock()
	r.index = s
	r.mu.Unlock()
}
func (r *Registry) Replace(list []Skill) {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	r.mu.Lock()
	r.sources = nil // an explicit in-memory registry no longer follows disk
	r.mu.Unlock()
	r.publish(list)
}
func (r *Registry) publish(list []Skill) {
	copy := append([]Skill(nil), list...)
	sort.SliceStable(copy, func(i, j int) bool { return copy[i].Name < copy[j].Name })
	s := buildSnapshot(copy)
	values := map[string]Skill{}
	for _, skill := range copy {
		values[skill.Name] = skill
	}
	r.mu.Lock()
	r.Skills = values
	r.index = s
	r.mu.Unlock()
}
func (r *Registry) snapshot() *snapshot {
	r.refreshIfDue()
	r.mu.RLock()
	s := r.index
	r.mu.RUnlock()
	if s != nil {
		return s
	}
	r.RebuildIndex()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.index
}
func search(s *snapshot, query string, k int) []Hit {
	terms := Tokenize(query)
	if len(terms) == 0 || len(s.skills) == 0 || k <= 0 {
		return nil
	}
	hits := []Hit{}
	for i, skill := range s.skills {
		if skill.Always {
			continue
		}
		score := 0.0
		for _, term := range terms {
			f := float64(s.frequencies[i][term])
			if f == 0 {
				continue
			}
			avg := s.average
			if avg == 0 {
				avg = 1
			}
			norm := BM25K1 * (1 - BM25B + BM25B*float64(s.lengths[i])/avg)
			score += s.idf[term] * f * (BM25K1 + 1) / (f + norm)
		}
		if score > 0 {
			hits = append(hits, Hit{Skill: skill, Score: score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > k {
		hits = hits[:k]
	}
	for i := range hits {
		hits[i].RRFScore = 1 / float64(RRFConstant+i+1)
	}
	return hits
}
func (r *Registry) Search(query string, k int) []Hit { return search(r.snapshot(), query, k) }

func explicit(query string, s Skill) bool {
	if s.Path == "" {
		return true
	}
	q := strings.ToLower(query)
	name := strings.TrimSpace(nameSeparators.ReplaceAllString(strings.ToLower(s.Name), " "))
	if utf8.RuneCountInString(name) >= 3 && strings.Contains(q, name) {
		return true
	}
	queryWords := map[string]bool{}
	for _, w := range nameWordsRE.FindAllString(q, -1) {
		queryWords[w] = true
	}
	for _, w := range nameWordsRE.FindAllString(name, -1) {
		if len(w) >= 3 && !stopWords[w] && queryWords[w] {
			return true
		}
	}
	return false
}
func relevant(query string, s Skill) bool {
	q := map[string]bool{}
	for _, w := range Tokenize(query) {
		q[w] = true
	}
	shared := map[string]bool{}
	for _, w := range Tokenize(s.Name + " " + s.Description + " " + firstRunes(s.Body, 2000)) {
		if q[w] {
			shared[w] = true
			if utf8.RuneCountInString(w) >= 2 && !stopWords[w] {
				return true
			}
		}
	}
	return len(shared) >= 2
}
func skillBlock(s Skill) string {
	return fmt.Sprintf("### Skill: %s [local/%s]\nSkill file: %s\n\n%s", s.Name, s.Name, s.Path, s.Body)
}
func (r *Registry) Resolve(query string) Resolution {
	return r.ResolveWithReferences(query, true)
}

func (r *Registry) ResolveWithReferences(query string, allowRead bool) Resolution {
	s := r.snapshot()
	out := Resolution{Metadata: SelectionMetadata{Method: "bm25_okapi_local"}}
	active := []string{}
	for _, skill := range s.skills {
		if skill.Always && len(active) < AlwaysLimit {
			active = append(active, skillBlock(skill))
			out.Metadata.AlwaysIDs = append(out.Metadata.AlwaysIDs, "local/"+skill.Name)
		}
	}
	if len(active) > 0 {
		out.ActiveText = "# Active Skills\n\n" + strings.Join(active, "\n\n")
	}
	// Single active source: over-fetch 2x, rank by positive BM25 and retain top-5.
	// RRF scores describe this one source; no remote source or LLM gate is implied.
	hits := search(s, query, CandidateLimit*2)
	if len(hits) > CandidateLimit {
		hits = hits[:CandidateLimit]
	}
	parts := []string{}
	refs := []string{}
	for _, hit := range hits {
		skill := hit.Skill
		id := "local/" + skill.Name
		out.Metadata.CandidateIDs = append(out.Metadata.CandidateIDs, id)
		if explicit(query, skill) {
			if len(out.Metadata.InjectedIDs) < ActivationLimit {
				parts = append(parts, skillBlock(skill))
				out.Metadata.InjectedIDs = append(out.Metadata.InjectedIDs, id)
			} else if allowRead {
				refs = append(refs, fmt.Sprintf("- %s: %s (use skill_read with name=%q)", skill.Name, skill.Description, skill.Name))
				out.Metadata.ReferencedIDs = append(out.Metadata.ReferencedIDs, id)
			} else {
				out.Metadata.UnavailableReferences = append(out.Metadata.UnavailableReferences, id)
			}
		} else if relevant(query, skill) && allowRead {
			refs = append(refs, fmt.Sprintf("- %s: %s (use skill_read with name=%q)", skill.Name, skill.Description, skill.Name))
			out.Metadata.ReferencedIDs = append(out.Metadata.ReferencedIDs, id)
		} else if relevant(query, skill) {
			out.Metadata.UnavailableReferences = append(out.Metadata.UnavailableReferences, id)
		}
	}
	if len(refs) > 0 {
		parts = append(parts, "Relevant skill references (body not loaded here):\n"+strings.Join(refs, "\n"))
	}
	if len(parts) > 0 {
		out.SelectedText = "# Skills\n\n" + strings.Join(parts, "\n\n")
	}
	return out
}
