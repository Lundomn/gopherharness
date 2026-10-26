package evaluation

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSWEImportExportsPredictionsWithoutGoldLeakage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cases.jsonl")
	row := map[string]any{"instance_id": "owner__repo-1", "repo": "owner/repo", "base_commit": strings.Repeat("a", 40), "problem_statement": "Fix this issue", "patch": "SECRET GOLD SOLUTION", "test_patch": "hidden tests", "FAIL_TO_PASS": "[\"test_fix\"]", "PASS_TO_PASS": []string{"test_existing"}}
	raw, _ := json.Marshal(row)
	_ = os.WriteFile(path, append(raw, '\n'), 0600)
	m, err := ImportSWE(path, "princeton-nlp/SWE-bench_Verified", "dataset-commit")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(m)
	if strings.Contains(string(encoded), "SECRET") || strings.Contains(string(encoded), "hidden tests") {
		t.Fatal("gold solution leaked into model manifest")
	}
	if len(m.Tasks) != 1 || m.Tasks[0].BaseCommit != strings.Repeat("a", 40) || len(m.Tasks[0].CheckoutPlan) != 2 {
		t.Fatal(m)
	}
	_ = os.WriteFile(filepath.Join(root, "owner__repo-1.patch"), []byte("diff --git a/a b/a\n"), 0600)
	var output bytes.Buffer
	if err := ExportSWEPredictions(m, root, "go-run", &output); err != nil {
		t.Fatal(err)
	}
	var prediction SWEPrediction
	if err := json.Unmarshal(output.Bytes(), &prediction); err != nil || prediction.ID != "owner__repo-1" || prediction.Model != "go-run" {
		t.Fatal(prediction, err)
	}
	if _, err := ReadPublicResults(m, filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing verifier became a success")
	}
	_ = os.Mkdir(filepath.Join(root, "owner__repo-1"), 0700)
	_ = os.WriteFile(filepath.Join(root, "owner__repo-1", "report.json"), []byte(`{"owner__repo-1":{"resolved":true,"patch_successfully_applied":true}}`), 0600)
	grade, err := ReadPublicResults(m, root)
	if err != nil || grade.Verified != 1 || grade.Origin != "imported_independent_verifier_artifacts" {
		t.Fatal(grade, err)
	}
	_ = os.WriteFile(filepath.Join(root, "owner__repo-1", "report.json"), []byte(`{"answer":"success"}`), 0600)
	if r, err := ReadPublicResults(m, root); err == nil || r.Missing != 1 {
		t.Fatal("response marker graded instead of official report", r, err)
	}
}

func TestSWERejectsUnpinnedDuplicateAndTraversal(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cases.jsonl")
	for _, id := range []string{"../escape", "valid"} {
		raw, _ := json.Marshal(map[string]any{"instance_id": id, "repo": "owner/repo", "base_commit": "main", "problem_statement": "p", "FAIL_TO_PASS": []string{"t"}, "PASS_TO_PASS": []string{}})
		_ = os.WriteFile(path, raw, 0600)
		if _, err := ImportSWE(path, "dataset", "rev"); err == nil {
			t.Fatal("accepted unsafe/unpinned case")
		}
	}
}

func TestHarborDirectoryAndIndependentRewards(t *testing.T) {
	root := t.TempDir()
	task := filepath.Join(root, "task-a")
	for _, dir := range []string{"environment", "tests"} {
		_ = os.MkdirAll(filepath.Join(task, dir), 0700)
	}
	for path, data := range map[string]string{"instruction.md": "Create a correct output file", "task.toml": "version = \"1.0\"", "environment/Dockerfile": "FROM busybox", "tests/test.sh": "echo 1 >/logs/verifier/reward.txt"} {
		_ = os.WriteFile(filepath.Join(task, path), []byte(data), 0600)
	}
	m, err := ImportTerminal(root, "fixed-dataset-revision")
	if err != nil || len(m.Tasks) != 1 || m.Tasks[0].Format != "harbor" {
		t.Fatal(m, err)
	}
	if _, err := ReadPublicResults(m, root); err == nil {
		t.Fatal("fixture tests are not executed verifier output")
	}
	reports := t.TempDir()
	_ = os.MkdirAll(filepath.Join(reports, "task-a", "verifier"), 0700)
	_ = os.WriteFile(filepath.Join(reports, "task-a", "verifier", "reward.json"), []byte(`{"reward":0.5}`), 0600)
	r, err := ReadPublicResults(m, reports)
	if err != nil || r.Failed != 1 || r.Verified != 0 || *r.Rows[0].Reward != .5 {
		t.Fatal(r, err)
	}
	_ = os.WriteFile(filepath.Join(reports, "task-a", "verifier", "reward.json"), []byte(`{"accuracy":1,"style":0.9}`), 0600)
	if _, err := ReadPublicResults(m, reports); err == nil {
		t.Fatal("invented aggregation for custom metrics")
	}
}

func TestLegacyTerminalImportNeedsRealVerifierFiles(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "task.yaml"), []byte("instruction: |-\n  Solve the real task\nauthor_name: ignored\n"), 0600)
	if _, err := ImportTerminal(root, "fixed"); err == nil {
		t.Fatal("missing independent verifier accepted")
	}
	_ = os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM busybox"), 0600)
	_ = os.WriteFile(filepath.Join(root, "run-tests.sh"), []byte("exit 0"), 0600)
	m, err := ImportTerminal(root, "fixed")
	if err != nil || m.Tasks[0].Prompt != "Solve the real task" {
		t.Fatal(m, err)
	}
}
