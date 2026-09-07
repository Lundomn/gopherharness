package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/evaluation"
	"github.com/Lundomn/gopherharness/internal/provider"
)

func main() {
	benchmark := flag.String("benchmark", "benchmarks/benchmark.json", "benchmark JSON path")
	fixtures := flag.String("fixtures", "", "fixture root")
	workspaces := flag.String("workspaces", "artifacts/go-workspaces", "workspace output root")
	artifact := flag.String("artifact", "artifacts/go-benchmark.json", "result artifact")
	providerName := flag.String("provider", "", "provider profile")
	flag.Parse()
	cwd, _ := os.Getwd()
	cfg, err := config.Load(cwd, "")
	if err != nil {
		die(err)
	}
	if *providerName != "" {
		cfg.Provider = *providerName
	}
	p, err := cfg.Selected()
	if err != nil {
		die(err)
	}
	client, err := provider.New(p, 300)
	if err != nil {
		die(err)
	}
	benchmarkPath, _ := filepath.Abs(*benchmark)
	result, err := evaluation.Run(context.Background(), evaluation.Options{BenchmarkPath: benchmarkPath, FixtureRoot: *fixtures, WorkspaceRoot: *workspaces, ArtifactPath: *artifact, Config: cfg, Provider: client})
	if err != nil {
		die(err)
	}
	fmt.Printf("passed=%d failed=%d artifact=%s\n", result.Passed, result.Failed, *artifact)
	if result.Failed > 0 {
		os.Exit(1)
	}
}
func die(err error) { fmt.Fprintln(os.Stderr, "gopherharness-eval:", err); os.Exit(1) }
