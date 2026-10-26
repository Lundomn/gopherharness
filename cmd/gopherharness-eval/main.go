package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/durable"
	"github.com/Lundomn/gopherharness/internal/evaluation"
	"github.com/Lundomn/gopherharness/internal/provider"
)

func main() {
	benchmark := flag.String("benchmark", "benchmarks/benchmark.json", "benchmark JSON path")
	fixtures := flag.String("fixtures", "", "fixture root")
	workspaces := flag.String("workspaces", "artifacts/go-workspaces", "workspace output root")
	artifact := flag.String("artifact", "artifacts/go-benchmark.json", "result artifact")
	providerName := flag.String("provider", "", "provider profile")
	offline := flag.Bool("offline", false, "use each task's deterministic fake model responses")
	allowVerifier := flag.Bool("allow-host-verifier", false, "explicitly allow trusted verifier scripts on this host; otherwise sandbox must be required")
	importSWE := flag.String("import-swebench", "", "import a local SWE-bench JSONL; no download, model or test execution")
	importTerminal := flag.String("import-terminal-bench", "", "import local Terminal-Bench/Harbor task directories; never run task scripts")
	dataset := flag.String("dataset", "princeton-nlp/SWE-bench_Verified", "public dataset identifier")
	revision := flag.String("dataset-revision", "", "pinned public dataset revision (required on import)")
	manifestPath := flag.String("public-manifest", "", "public manifest output on import, input on export/verify")
	patches := flag.String("export-swe-patches", "", "directory of <instance_id>.patch files to export as official predictions JSONL")
	modelIdentity := flag.String("model-identity", "", "model/run identity for public predictions")
	predictions := flag.String("predictions", "artifacts/public-predictions.jsonl", "official SWE predictions output")
	reports := flag.String("verify-public-reports", "", "official reports root: <instance>/report.json or <task>/verifier/reward.txt")
	flag.Parse()
	modeCount := 0
	for _, s := range []string{*importSWE, *importTerminal, *patches, *reports} {
		if s != "" {
			modeCount++
		}
	}
	if modeCount > 1 {
		die(fmt.Errorf("select only one public import/export/verification mode"))
	}
	if modeCount == 1 {
		if *manifestPath == "" {
			die(fmt.Errorf("public mode requires -public-manifest"))
		}
		var manifest evaluation.PublicManifest
		var err error
		switch {
		case *importSWE != "":
			manifest, err = evaluation.ImportSWE(*importSWE, *dataset, *revision)
		case *importTerminal != "":
			manifest, err = evaluation.ImportTerminal(*importTerminal, *revision)
		default:
			manifest, err = evaluation.LoadPublicManifest(*manifestPath)
		}
		if err != nil {
			die(err)
		}
		if *importSWE != "" || *importTerminal != "" {
			if err = evaluation.WritePublicManifest(*manifestPath, manifest); err != nil {
				die(err)
			}
			fmt.Printf("imported=%d; checkout plans only; official verifier still required; manifest=%s\n", len(manifest.Tasks), *manifestPath)
			return
		}
		if *patches != "" {
			var output bytes.Buffer
			if err = evaluation.ExportSWEPredictions(manifest, *patches, *modelIdentity, &output); err != nil {
				die(err)
			}
			if err = durable.Replace(*predictions, output.Bytes()); err != nil {
				die(err)
			}
			fmt.Printf("predictions=%s; grade with the official Docker harness, not response matching\n", *predictions)
			return
		}
		report, err := evaluation.ReadPublicResults(manifest, *reports)
		raw, marshalErr := json.MarshalIndent(report, "", "  ")
		if marshalErr != nil {
			die(marshalErr)
		}
		if e := durable.Replace(*artifact, append(raw, '\n')); e != nil {
			die(e)
		}
		if err != nil {
			die(err)
		}
		fmt.Printf("imported official artifacts: verified=%d failed=%d missing=%d\n", report.Verified, report.Failed, report.Missing)
		if report.Failed > 0 {
			os.Exit(1)
		}
		return
	}
	cwd, _ := os.Getwd()
	cfg, err := config.Load(cwd, "")
	if err != nil {
		die(err)
	}
	if *providerName != "" {
		cfg.Provider = *providerName
	}
	var client provider.Client
	if !*offline {
		p, err := cfg.Selected()
		if err != nil {
			die(err)
		}
		client, err = provider.New(p, 300)
		if err != nil {
			die(err)
		}
	}
	benchmarkPath, _ := filepath.Abs(*benchmark)
	result, err := evaluation.Run(context.Background(), evaluation.Options{BenchmarkPath: benchmarkPath, FixtureRoot: *fixtures, WorkspaceRoot: *workspaces, ArtifactPath: *artifact, Config: cfg, Provider: client, AllowHostVerifier: *allowVerifier})
	if err != nil {
		die(err)
	}
	fmt.Printf("passed=%d failed=%d artifact=%s\n", result.Passed, result.Failed, *artifact)
	if result.Failed > 0 {
		os.Exit(1)
	}
}
func die(err error) { fmt.Fprintln(os.Stderr, "gopherharness-eval:", err); os.Exit(1) }
