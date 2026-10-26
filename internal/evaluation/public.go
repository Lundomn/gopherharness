package evaluation

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// PublicManifest is an import/export contract, deliberately separate from the
// fixed-response runtime regression suite. Importing never runs dataset code,
// downloads repositories, or counts a model's answer as a verified solution.
type PublicManifest struct {
	Schema       string       `json:"schema"`
	Benchmark    string       `json:"benchmark"`
	Dataset      string       `json:"dataset"`
	Revision     string       `json:"revision"`
	SourceDigest string       `json:"source_digest"`
	SourceRoot   string       `json:"source_root,omitempty"`
	Tasks        []PublicTask `json:"tasks"`
}
type PublicTask struct {
	ID           string     `json:"id"`
	Prompt       string     `json:"prompt"`
	Repository   string     `json:"repository,omitempty"`
	BaseCommit   string     `json:"base_commit,omitempty"`
	Directory    string     `json:"directory,omitempty"`
	Format       string     `json:"format"`
	Verifier     string     `json:"verifier"`
	FailToPass   []string   `json:"fail_to_pass,omitempty"`
	PassToPass   []string   `json:"pass_to_pass,omitempty"`
	CheckoutPlan [][]string `json:"checkout_plan,omitempty"`
}

var safePublicID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var repositoryID = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var fullCommit = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

func ImportSWE(path, dataset, revision string) (PublicManifest, error) {
	m := PublicManifest{Schema: "gopherharness.public.v1", Benchmark: "swe-bench", Dataset: dataset, Revision: revision}
	if strings.TrimSpace(dataset) == "" || strings.TrimSpace(revision) == "" {
		return m, errors.New("dataset name and pinned dataset revision are required")
	}
	data, err := readArtifactPath(path, 64<<20)
	if err != nil {
		return m, err
	}
	m.SourceDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	scan := bufio.NewScanner(bytes.NewReader(data))
	scan.Buffer(make([]byte, 64<<10), 8<<20)
	seen := map[string]bool{}
	for line := 1; scan.Scan(); line++ {
		if len(bytes.TrimSpace(scan.Bytes())) == 0 {
			continue
		}
		var input struct {
			ID     string          `json:"instance_id"`
			Prompt string          `json:"problem_statement"`
			Repo   string          `json:"repo"`
			Base   string          `json:"base_commit"`
			Fail   json.RawMessage `json:"FAIL_TO_PASS"`
			Pass   json.RawMessage `json:"PASS_TO_PASS"`
		}
		if err := json.Unmarshal(scan.Bytes(), &input); err != nil {
			return m, fmt.Errorf("SWE-bench line %d: %w", line, err)
		}
		if !safePublicID.MatchString(input.ID) || seen[input.ID] || !repositoryID.MatchString(input.Repo) || !fullCommit.MatchString(input.Base) || strings.TrimSpace(input.Prompt) == "" {
			return m, fmt.Errorf("SWE-bench line %d has duplicate/unsafe identity, missing problem or non-pinned base commit", line)
		}
		seen[input.ID] = true
		fail, err := testNames(input.Fail)
		if err != nil {
			return m, fmt.Errorf("%s FAIL_TO_PASS: %w", input.ID, err)
		}
		pass, err := testNames(input.Pass)
		if err != nil {
			return m, fmt.Errorf("%s PASS_TO_PASS: %w", input.ID, err)
		}
		if len(fail) == 0 {
			return m, fmt.Errorf("%s has no FAIL_TO_PASS tests; official grading metadata required", input.ID)
		}
		t := PublicTask{ID: input.ID, Prompt: input.Prompt, Repository: input.Repo, BaseCommit: input.Base, Format: "swe-bench", Verifier: "swebench.harness.run_evaluation (official Docker harness)", FailToPass: fail, PassToPass: pass}
		// This is a printed plan, never executed by the importer. Each task gets
		// a new checkout; no reset/checkout is run in an existing user repository.
		t.CheckoutPlan = [][]string{{"git", "clone", "--no-checkout", "https://github.com/" + input.Repo + ".git", input.ID}, {"git", "-C", input.ID, "checkout", "--detach", input.Base}}
		m.Tasks = append(m.Tasks, t)
	}
	if err := scan.Err(); err != nil {
		return m, err
	}
	if len(m.Tasks) == 0 {
		return m, errors.New("public dataset contains no tasks")
	}
	return m, nil
}
func testNames(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, errors.New("missing test list")
	}
	if raw[0] == '"' {
		var nested string
		if err := json.Unmarshal(raw, &nested); err != nil {
			return nil, err
		}
		raw = []byte(nested)
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, err
	}
	return names, nil
}

func ImportTerminal(root, revision string) (PublicManifest, error) {
	m := PublicManifest{Schema: "gopherharness.public.v1", Benchmark: "terminal-bench", Dataset: "local-terminal-bench", Revision: revision}
	if strings.TrimSpace(revision) == "" {
		return m, errors.New("pinned task dataset revision required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return m, err
	}
	m.SourceRoot = abs
	confined, err := os.OpenRoot(abs)
	if err != nil {
		return m, err
	}
	defer confined.Close()
	entries, err := os.ReadDir(abs)
	if err != nil {
		return m, err
	}
	dirs := []string{}
	if fileExists(filepath.Join(abs, "instruction.md")) || fileExists(filepath.Join(abs, "task.yaml")) {
		dirs = []string{"."}
	} else {
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, e.Name())
			}
		}
	}
	for _, dir := range dirs {
		base := filepath.Join(abs, dir)
		id := filepath.Base(base)
		if !safePublicID.MatchString(id) {
			return m, fmt.Errorf("unsafe task ID %q", id)
		}
		t := PublicTask{ID: id, Directory: dir}
		switch {
		case fileExists(filepath.Join(base, "instruction.md")):
			prompt, err := readArtifact(confined, filepath.Join(dir, "instruction.md"), 1<<20)
			if err != nil {
				return m, err
			}
			t.Prompt = string(prompt)
			t.Format = "harbor"
			if !fileExists(filepath.Join(base, "task.toml")) || !fileExists(filepath.Join(base, "tests", "test.sh")) {
				return m, fmt.Errorf("%s requires task.toml and independent tests/test.sh", id)
			}
			if !fileExists(filepath.Join(base, "environment", "Dockerfile")) {
				return m, fmt.Errorf("%s environment/Dockerfile missing; image-only/remote environments require a dedicated adapter", id)
			}
			t.Verifier = "harbor verifier: tests/test.sh -> /logs/verifier/reward.txt or reward.json"
		case fileExists(filepath.Join(base, "task.yaml")):
			data, err := readArtifact(confined, filepath.Join(dir, "task.yaml"), 1<<20)
			if err != nil {
				return m, err
			}
			t.Prompt, err = legacyInstruction(data)
			if err != nil {
				return m, fmt.Errorf("%s: %w", id, err)
			}
			t.Format = "terminal-bench-v1"
			if !fileExists(filepath.Join(base, "run-tests.sh")) || (!fileExists(filepath.Join(base, "Dockerfile")) && !fileExists(filepath.Join(base, "docker-compose.yaml"))) {
				return m, fmt.Errorf("%s requires run-tests.sh and Docker environment", id)
			}
			t.Verifier = "Terminal-Bench official harness: run-tests.sh and tests/"
		default:
			continue
		}
		if strings.TrimSpace(t.Prompt) == "" {
			return m, fmt.Errorf("%s has empty instructions", id)
		}
		m.Tasks = append(m.Tasks, t)
	}
	if len(m.Tasks) == 0 {
		return m, errors.New("no supported Terminal-Bench tasks found")
	}
	m.SourceDigest, err = treeDigest(abs)
	return m, err
}
func fileExists(path string) bool { s, err := os.Stat(path); return err == nil && s.Mode().IsRegular() }
func legacyInstruction(data []byte) (string, error) {
	// The v1 published format uses an indented literal block. Unsupported YAML
	// constructs are rejected rather than silently misinterpreting task prose.
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "instruction:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "instruction:"))
		if value != "|" && value != "|-" && value != "|+" {
			return "", errors.New("v1 importer requires instruction: | literal block; convert other YAML forms explicitly")
		}
		var text []string
		for _, part := range lines[i+1:] {
			if strings.TrimSpace(part) != "" && !strings.HasPrefix(part, " ") {
				break
			}
			text = append(text, strings.TrimPrefix(part, "  "))
		}
		return strings.TrimSpace(strings.Join(text, "\n")), nil
	}
	return "", errors.New("task.yaml instruction missing")
}

func WritePublicManifest(path string, m PublicManifest) error { return writeJSON(path, m) }
func LoadPublicManifest(path string) (PublicManifest, error) {
	var m PublicManifest
	data, err := readArtifactPath(path, 64<<20)
	if err != nil {
		return m, err
	}
	if err = json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	if m.Schema != "gopherharness.public.v1" || m.Revision == "" || m.SourceDigest == "" || len(m.Tasks) == 0 {
		return m, errors.New("invalid or unpinned public manifest")
	}
	seen := map[string]bool{}
	for _, t := range m.Tasks {
		if !safePublicID.MatchString(t.ID) || seen[t.ID] {
			return m, errors.New("invalid or duplicate public task ID")
		}
		seen[t.ID] = true
	}
	return m, nil
}

type SWEPrediction struct {
	ID    string `json:"instance_id"`
	Model string `json:"model_name_or_path"`
	Patch string `json:"model_patch"`
}

func ExportSWEPredictions(m PublicManifest, patchRoot, modelName string, out io.Writer) error {
	if m.Benchmark != "swe-bench" || strings.TrimSpace(modelName) == "" {
		return errors.New("SWE-bench manifest and model identity required")
	}
	r, err := os.OpenRoot(patchRoot)
	if err != nil {
		return err
	}
	defer r.Close()
	var predictions []SWEPrediction
	total := 0
	for _, t := range m.Tasks {
		if !safePublicID.MatchString(t.ID) {
			return errors.New("unsafe instance ID")
		}
		data, err := readArtifact(r, t.ID+".patch", 16<<20)
		if err != nil {
			return fmt.Errorf("%s patch: %w", t.ID, err)
		}
		total += len(data)
		if total > 64<<20 {
			return errors.New("prediction batch exceeds 64 MiB; export a smaller manifest")
		}
		predictions = append(predictions, SWEPrediction{ID: t.ID, Model: modelName, Patch: string(data)})
	}
	enc := json.NewEncoder(out)
	for _, p := range predictions {
		if err := enc.Encode(p); err != nil {
			return err
		}
	}
	return nil
}

type PublicVerdict struct {
	ID               string   `json:"id"`
	Status           string   `json:"status"`
	Reward           *float64 `json:"reward,omitempty"`
	VerifierArtifact string   `json:"verifier_artifact"`
	Detail           string   `json:"detail,omitempty"`
}
type PublicReport struct {
	Schema       string          `json:"schema"`
	Benchmark    string          `json:"benchmark"`
	Dataset      string          `json:"dataset"`
	Revision     string          `json:"revision"`
	SourceDigest string          `json:"source_digest"`
	Origin       string          `json:"origin"`
	Verified     int             `json:"verified"`
	Failed       int             `json:"failed"`
	Missing      int             `json:"missing"`
	Rows         []PublicVerdict `json:"rows"`
}

// ReadPublicResults accepts independent verifier artifacts. It never inspects
// the assistant's answer for success markers. Missing official resources fail
// explicitly; these artifacts are imported results, not tests run by this API.
func ReadPublicResults(m PublicManifest, reports string) (PublicReport, error) {
	r := PublicReport{Schema: "gopherharness.public-results.v1", Benchmark: m.Benchmark, Dataset: m.Dataset, Revision: m.Revision, SourceDigest: m.SourceDigest, Origin: "imported_independent_verifier_artifacts"}
	root, err := os.OpenRoot(reports)
	if err != nil {
		return r, fmt.Errorf("official verifier output is required: %w", err)
	}
	defer root.Close()
	for _, task := range m.Tasks {
		if !safePublicID.MatchString(task.ID) {
			return r, errors.New("unsafe instance ID")
		}
		v := PublicVerdict{ID: task.ID, Status: "missing_verifier"}
		switch task.Format {
		case "swe-bench":
			v.VerifierArtifact = filepath.Join(task.ID, "report.json")
			data, e := readArtifact(root, v.VerifierArtifact, 1<<20)
			if e == nil {
				var report map[string]struct {
					Resolved *bool `json:"resolved"`
					Applied  *bool `json:"patch_successfully_applied"`
				}
				if e = json.Unmarshal(data, &report); e == nil {
					item, ok := report[task.ID]
					if !ok || item.Resolved == nil {
						e = errors.New("official per-instance resolved field missing")
					} else if *item.Resolved && item.Applied != nil && !*item.Applied {
						e = errors.New("contradictory patch application/result")
					} else if *item.Resolved {
						v.Status = "verified"
					} else {
						v.Status = "failed"
					}
				}
			}
			if e != nil {
				v.Detail = e.Error()
			}
		case "harbor":
			v.VerifierArtifact = filepath.Join(task.ID, "verifier", "reward.txt")
			data, e := readArtifact(root, v.VerifierArtifact, 1<<20)
			if os.IsNotExist(e) {
				v.VerifierArtifact = filepath.Join(task.ID, "verifier", "reward.json")
				var object map[string]json.RawMessage
				if data, e = readArtifact(root, v.VerifierArtifact, 1<<20); e == nil {
					if e = json.Unmarshal(data, &object); e == nil {
						if raw, ok := object["reward"]; ok {
							data = raw
						} else {
							e = errors.New("reward.json has multiple/custom metrics; an explicit aggregation contract is required")
						}
					}
				}
			}
			if e == nil {
				score, parseErr := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
				if parseErr == nil && score >= 0 && score <= 1 {
					v.Reward = &score
					if score == 1 {
						v.Status = "verified"
					} else {
						v.Status = "failed"
					}
				} else {
					v.Detail = "reward.txt must contain a finite success score in [0,1]"
				}
			} else {
				v.Detail = e.Error()
			}
		case "terminal-bench-v1":
			v.Detail = "v1 verifier results require its official harness parser; no generic exit-code/answer fallback is permitted"
		default:
			v.Detail = "unsupported verifier format"
		}
		switch v.Status {
		case "verified":
			r.Verified++
		case "failed":
			r.Failed++
		default:
			r.Missing++
		}
		r.Rows = append(r.Rows, v)
	}
	sort.Slice(r.Rows, func(i, j int) bool { return r.Rows[i].ID < r.Rows[j].ID })
	if r.Missing > 0 {
		return r, fmt.Errorf("%d tasks lack valid independent verifier artifacts; no official score can be reported", r.Missing)
	}
	return r, nil
}
