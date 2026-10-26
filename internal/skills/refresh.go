package skills

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const RefreshInterval = time.Second
const MaxSkillBytes = 1 << 20

// DiscoverScoped can exclude personal home-directory skills for shared services.
// Each registry has a workspace-specific immutable index. Project sources take
// precedence over global names: global < .agents < .pico.
func DiscoverScoped(root string, includeUser bool) *Registry {
	dirs := []string{}
	if includeUser {
		if home, err := os.UserHomeDir(); err == nil {
			dirs = append(dirs, filepath.Join(home, ".config", "pico", "skills"))
		}
	}
	dirs = append(dirs, filepath.Join(root, ".agents", "skills"), filepath.Join(root, ".pico", "skills"))
	r := &Registry{Skills: map[string]Skill{}, sources: dirs}
	_ = r.Refresh()
	return r
}

// Refresh publishes the complete valid snapshot, including deletions. Invalid
// files are removed from the current catalogue instead of retaining permission-
// revoked stale bodies. Errors remain observable through LastRefreshError.
func (r *Registry) Refresh() error {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	return r.refreshLocked()
}
func (r *Registry) refreshIfDue() {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	r.mu.RLock()
	due := len(r.sources) > 0 && time.Since(r.lastRefresh) >= RefreshInterval
	r.mu.RUnlock()
	if due {
		_ = r.refreshLocked()
	}
}
func (r *Registry) refreshLocked() error {
	r.mu.RLock()
	sources := append([]string(nil), r.sources...)
	r.mu.RUnlock()
	if len(sources) == 0 {
		return nil
	}
	values := map[string]Skill{}
	var refreshErr error
	for _, base := range sources {
		root, err := os.OpenRoot(base)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			refreshErr = errors.Join(refreshErr, err)
			continue
		}
		dir, err := root.Open(".")
		if err != nil {
			_ = root.Close()
			refreshErr = errors.Join(refreshErr, err)
			continue
		}
		entries, err := dir.ReadDir(-1)
		_ = dir.Close()
		if err != nil {
			_ = root.Close()
			refreshErr = errors.Join(refreshErr, err)
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			rel := filepath.Join(entry.Name(), "SKILL.md")
			file, err := openSkillFile(root, rel)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				refreshErr = errors.Join(refreshErr, fmt.Errorf("skill %s: %w", rel, err))
				continue
			}
			info, err := file.Stat()
			if err == nil && (!info.Mode().IsRegular() || info.Size() > MaxSkillBytes) {
				err = fmt.Errorf("skill must be regular and at most %d bytes", MaxSkillBytes)
			}
			var body []byte
			if err == nil {
				body, err = io.ReadAll(io.LimitReader(file, MaxSkillBytes+1))
			}
			_ = file.Close()
			if err == nil && len(body) > MaxSkillBytes {
				err = fmt.Errorf("skill exceeds %d bytes", MaxSkillBytes)
			}
			if err != nil {
				refreshErr = errors.Join(refreshErr, fmt.Errorf("skill %s: %w", rel, err))
				continue
			}
			skill := parse(filepath.Join(base, rel), string(body), entry.Name())
			if skill.Name == "" || skill.Name == "." || skill.Name == ".." || strings.ContainsAny(skill.Name, "/\\\n\r") {
				refreshErr = errors.Join(refreshErr, fmt.Errorf("invalid skill name in %s", rel))
				continue
			}
			values[skill.Name] = skill
		}
		_ = root.Close()
	}
	list := make([]Skill, 0, len(values))
	for _, skill := range values {
		list = append(list, skill)
	}
	r.publish(list)
	r.mu.Lock()
	r.lastRefresh = time.Now()
	r.refreshErr = refreshErr
	r.mu.Unlock()
	return refreshErr
}
func (r *Registry) LastRefreshError() error { r.mu.RLock(); defer r.mu.RUnlock(); return r.refreshErr }
