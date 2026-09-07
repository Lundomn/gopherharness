package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverAndSelectSkill(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".pico", "skills", "testing")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := "---\nname: testing\ndescription: Run repository tests safely\n---\n# Testing\nAlways inspect failures first.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Discover(root)
	if len(r.List()) != 1 {
		t.Fatalf("skills=%#v", r.List())
	}
	rendered := r.Render("please use testing to run repository tests safely")
	if !strings.Contains(rendered, "Always inspect failures first") {
		t.Fatalf("skill body not selected: %s", rendered)
	}
}
