package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOffRunnerExecutesCommand(t *testing.T) {
	r := New(Config{Mode: "off", Backend: "auto", WorkspaceWrite: true})
	out, err := r.Run(context.Background(), "printf hello", t.TempDir(), []string{"PATH=/usr/bin:/bin"}, 10)
	if err != nil || out.ExitCode != 0 || strings.TrimSpace(out.Stdout) != "hello" || out.Backend != "none" {
		t.Fatalf("out=%#v err=%v", out, err)
	}
}

func TestRunnerReportsTimeoutInsteadOfSuccessfulExit(t *testing.T) {
	r := New(Config{Mode: "off", Backend: "none", WorkspaceWrite: true})
	started := time.Now()
	result, err := r.Run(context.Background(), "sleep 2", t.TempDir(), []string{"PATH=/usr/bin:/bin"}, 1)
	if err == nil || !strings.Contains(err.Error(), "shell timeout") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if time.Since(started) > 1500*time.Millisecond {
		t.Fatalf("timeout took too long: %s", time.Since(started))
	}
}
func TestRequiredSandboxFailsWithoutBackendOnNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		t.Skip("platform has a supported sandbox contract")
	}
	r := New(Config{Mode: "required", Backend: "auto"})
	if _, err := r.Run(context.Background(), "true", t.TempDir(), nil, 1); err == nil {
		t.Fatal("required sandbox unexpectedly degraded")
	}
}

func TestMacOSSandboxRestrictsWrites(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	workspace := t.TempDir()
	outside := filepath.Join(filepath.Dir(workspace), "outside-sandbox-probe")
	_ = os.Remove(outside)
	r := New(Config{Mode: "required", Backend: "auto", WorkspaceWrite: true})
	result, err := r.Run(context.Background(), "touch allowed.txt; touch '"+outside+"'", workspace, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Backend != "sandbox-exec" || result.ExitCode == 0 {
		t.Fatalf("sandbox did not reject outside write: %#v", result)
	}
	if _, err = os.Stat(filepath.Join(workspace, "allowed.txt")); err != nil {
		t.Fatal("workspace write should be allowed")
	}
	if _, err = os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("sandbox wrote outside workspace")
	}

	readOnly := New(Config{Mode: "required", Backend: "auto", WorkspaceWrite: false})
	result, err = readOnly.Run(context.Background(), "touch denied.txt", workspace, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 {
		t.Fatal("read-only sandbox allowed workspace write")
	}
}
