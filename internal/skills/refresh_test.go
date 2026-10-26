package skills

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestSkill(t *testing.T, root, source, body string) string {
	t.Helper()
	path := filepath.Join(root, source, "skills", "reader", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: reader\ndescription: pdf documents\n---\n"+body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestRefreshReplacesBodiesDeletesEntriesAndKeepsWorkspaceScope(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	path := writeTestSkill(t, one, ".pico", "OLD")
	writeTestSkill(t, two, ".pico", "OTHER_PROJECT")
	a, b := DiscoverScoped(one, false), DiscoverScoped(two, false)
	if got, _ := a.Get("reader"); got.Body != "OLD" {
		t.Fatal(got)
	}
	if got, _ := b.Get("reader"); got.Body != "OTHER_PROJECT" {
		t.Fatal(got)
	}
	writeTestSkill(t, one, ".pico", "NEW")
	a.mu.Lock()
	a.lastRefresh = time.Now().Add(-2 * RefreshInterval)
	a.mu.Unlock()
	if hits := a.Search("pdf", 1); len(hits) != 1 || hits[0].Skill.Body != "NEW" {
		t.Fatal(hits)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Get("reader"); ok {
		t.Fatal("deleted skill remained callable")
	}
	if got, _ := b.Get("reader"); got.Body != "OTHER_PROJECT" {
		t.Fatal("other scope mutated")
	}
}
func TestProjectSkillsOverrideSharedDirectoryAndRejectEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	writeTestSkill(t, root, ".agents", "SHARED")
	path := writeTestSkill(t, root, ".pico", "PROJECT")
	r := DiscoverScoped(root, false)
	if got, _ := r.Get("reader"); got.Body != "PROJECT" {
		t.Fatal(got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.md")
	_ = os.WriteFile(outside, []byte("SECRET"), 0600)
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := r.Refresh(); err == nil {
		t.Fatal("escaping skill symlink silently accepted")
	}
	if got, _ := r.Get("reader"); got.Body == "SECRET" || got.Body == "PROJECT" {
		t.Fatal("unsafe or revoked body retained")
	}
}
