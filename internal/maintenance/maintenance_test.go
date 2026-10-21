package maintenance

import (
	"context"
	"github.com/Lundomn/gopherharness/internal/durable"
	"os"
	"path/filepath"
	"testing"
)

func TestFixUsesIndependentPinnedWorktrees(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if _, e := git(ctx, root, "init"); e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{{"config", "user.email", "test@example.invalid"}, {"config", "user.name", "fixture"}} {
		if _, e := git(ctx, root, args...); e != nil {
			t.Fatal(e)
		}
	}
	if e := durable.Replace(filepath.Join(root, "code.txt"), []byte("bad\n")); e != nil {
		t.Fatal(e)
	}
	_, _ = git(ctx, root, "add", "code.txt")
	if _, e := git(ctx, root, "commit", "-m", "fixture"); e != nil {
		t.Fatal(e)
	}
	m := Manager{Config: Config{Root: root, VerifyCommands: []string{"test \"$(cat code.txt)\" = good"}}, Repair: func(ctx context.Context, dir, q string) error {
		return durable.Replace(filepath.Join(dir, "code.txt"), []byte("good\n"))
	}}
	r, e := m.Fix(ctx, "fix it")
	if e != nil || r.Status != "candidate_ready" {
		t.Fatal(r, e)
	}
	b, _ := os.ReadFile(filepath.Join(root, "code.txt"))
	if string(b) != "bad\n" || r.RepairPath == r.VerifierPath {
		t.Fatal("repair polluted source or verifier not isolated")
	}
	if _, e = os.Stat(r.PatchPath); e != nil {
		t.Fatal(e)
	}
	m.Config.VerifyCommands = []string{"exit 9"}
	r, e = m.Fix(ctx, "fails acceptance")
	if e == nil || r.Status != "verification_failed" {
		t.Fatal(r, e)
	}
}
