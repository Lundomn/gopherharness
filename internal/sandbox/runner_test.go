package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
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

func TestIsolationCannotSilentlyDegrade(t *testing.T) {
	for _, cfg := range []Config{
		{Mode: "required", Backend: "none"},
		{Mode: "off", Backend: "auto", DenyNetwork: true},
		{Mode: "best_effort", Backend: "none", DenyNetwork: true},
		{Mode: "typo", Backend: "auto"},
	} {
		if err := New(cfg).Validate(); err == nil {
			t.Fatalf("unexpectedly accepted %#v", cfg)
		}
	}
}

func TestOutputBoundDoesNotHideFailedCommand(t *testing.T) {
	r := New(Config{Mode: "off"})
	result, err := r.Run(context.Background(), "printf BEGIN; head -c 1200000 /dev/zero; printf END; printf ERRBEGIN >&2; head -c 1200000 /dev/zero >&2; printf ERREND >&2; exit 7", t.TempDir(), []string{"PATH=/usr/bin:/bin"}, 10)
	if err != nil || result.ExitCode != 7 {
		t.Fatalf("exit=%d err=%v", result.ExitCode, err)
	}
	if !result.StdoutTruncated || !result.StderrTruncated || result.StdoutDroppedBytes == 0 || result.StderrDroppedBytes == 0 || len(result.Stdout)+len(result.Stderr) > MaxOutputBytes {
		t.Fatalf("incorrect truncation: stdout=%d stderr=%d dropped=%d/%d", len(result.Stdout), len(result.Stderr), result.StdoutDroppedBytes, result.StderrDroppedBytes)
	}
	if !strings.HasPrefix(result.Stdout, "BEGIN") || !strings.HasSuffix(result.Stdout, "END") || !strings.HasSuffix(result.Stderr, "ERREND") {
		t.Fatal("head or tail lost")
	}
}

func TestBoundedOutputRetainsSmallChunksAndEnds(t *testing.T) {
	b := newBoundedOutput(128)
	_, _ = b.Write([]byte("first"))
	if s, n := b.snapshot(); s != "first" || n != 0 {
		t.Fatalf("small capture %q omitted=%d", s, n)
	}
	for i := 0; i < 300; i++ {
		_, _ = b.Write([]byte("x"))
	}
	_, _ = b.Write([]byte("last"))
	s, n := b.snapshot()
	if len(s) != 128 || n == 0 || !strings.HasPrefix(s, "first") || !strings.HasSuffix(s, "last") {
		t.Fatalf("head/tail capture %q omitted=%d", s, n)
	}
}

func TestMacOSSandboxDeniesNetworkWhenRequested(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not installed")
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); _, _ = w.Write([]byte("ok")) }))
	defer server.Close()
	r := New(Config{Mode: "required", DenyNetwork: true})
	result, err := r.Run(context.Background(), curl+" -sS --max-time 2 "+server.URL, t.TempDir(), []string{"PATH=/usr/bin:/bin"}, 5)
	if err != nil || result.ExitCode == 0 || requests.Load() != 0 {
		t.Fatalf("network boundary: exit=%d requests=%d err=%v stderr=%s", result.ExitCode, requests.Load(), err, result.Stderr)
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
