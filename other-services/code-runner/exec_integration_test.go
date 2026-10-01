//go:build sandbox_integration

package main

// Adversarial integration tests for the per-run enforcement. They spawn REAL
// programs, so they're gated behind the `sandbox_integration` build tag and
// excluded from the default hermetic `go test`. Run where python3 + /bin/sh are
// available:
//
//	go test -tags sandbox_integration ./other-services/code-runner/
//
// These validate the security-critical kill paths (timeout, output cap, exit
// status, artifact collection, workdir cleanup). Full container isolation
// (network=none, OOM cgroup kill, seccomp escape) is validated separately in a
// Docker/gVisor environment against the built image — see the code-runner service in
// final-compose.yml (profile code-execution).

import (
	"os"
	"strings"
	"testing"
	"time"
)

func pyLimits(wall time.Duration, outBytes int64) Limits {
	return Limits{
		Wall:          int64(wall),
		CPU:           int64(10 * time.Second),
		MemoryBytes:   512 << 20,
		PIDs:          64,
		OutputBytes:   outBytes,
		MaxArtifacts:  8,
		ArtifactBytes: 2 << 20,
	}
}

func runPy(t *testing.T, code string, l Limits) Result {
	t.Helper()
	t.Setenv("CODE_RUNNER_WORKDIR", t.TempDir())
	res, err := runJob(Job{ID: "it", Language: "python", Code: code, Limits: l})
	if err != nil {
		t.Fatalf("runJob setup error: %v", err)
	}
	return res
}

func TestIntegration_OK(t *testing.T) {
	res := runPy(t, "print('hello world')", pyLimits(10*time.Second, 1<<20))
	if res.Status != StatusOK {
		t.Fatalf("status=%s stderr=%s", res.Status, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "hello world") {
		t.Fatalf("stdout=%q", res.Stdout)
	}
}

func TestIntegration_TimeoutKillsBusyLoop(t *testing.T) {
	start := time.Now()
	res := runPy(t, "while True:\n    pass\n", pyLimits(1*time.Second, 1<<20))
	if res.Status != StatusTimeout {
		t.Fatalf("expected timeout, got %s", res.Status)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout did not kill promptly")
	}
}

func TestIntegration_OutputCap(t *testing.T) {
	// Flood stdout far past a tiny cap.
	res := runPy(t, "print('x' * 10_000_000)", pyLimits(10*time.Second, 1024))
	if res.Status != StatusKilledLimit {
		t.Fatalf("expected killed_limit, got %s", res.Status)
	}
	if len(res.Stdout) > 1024 {
		t.Fatalf("stdout not capped: %d bytes", len(res.Stdout))
	}
}

func TestIntegration_ExitCodeIsError(t *testing.T) {
	res := runPy(t, "import sys\nsys.exit(3)\n", pyLimits(10*time.Second, 1<<20))
	if res.Status != StatusError {
		t.Fatalf("expected error, got %s", res.Status)
	}
}

func TestIntegration_RaiseIsError(t *testing.T) {
	res := runPy(t, "raise ValueError('boom')", pyLimits(10*time.Second, 1<<20))
	if res.Status != StatusError {
		t.Fatalf("expected error, got %s", res.Status)
	}
	if !strings.Contains(res.Stderr, "ValueError") {
		t.Fatalf("stderr should carry the exception: %q", res.Stderr)
	}
}

func TestIntegration_CollectsChartArtifact(t *testing.T) {
	code := "import os\nos.makedirs('out', exist_ok=True)\nopen('out/chart.json','w').write('{\"type\":\"bar\",\"labels\":[\"a\"],\"series\":[]}')\nprint('done')\n"
	res := runPy(t, code, pyLimits(10*time.Second, 1<<20))
	if res.Status != StatusOK {
		t.Fatalf("status=%s stderr=%s", res.Status, res.Stderr)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Kind != KindChart {
		t.Fatalf("expected one chart artifact, got %+v", res.Artifacts)
	}
	if !strings.Contains(string(res.Artifacts[0].Bytes), `"type":"bar"`) {
		t.Fatalf("artifact content wrong: %s", res.Artifacts[0].Bytes)
	}
}

func TestIntegration_ReadsInjectedInput(t *testing.T) {
	job := Job{
		ID:       "it",
		Language: "python",
		Code:     "print(open('data/nums.csv').read().strip())",
		Files:    map[string][]byte{"nums.csv": []byte("a,b\n1,2")},
		Limits:   pyLimits(10*time.Second, 1<<20),
	}
	t.Setenv("CODE_RUNNER_WORKDIR", t.TempDir())
	res, err := runJob(job)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK || !strings.Contains(res.Stdout, "1,2") {
		t.Fatalf("injected input not readable: status=%s stdout=%q stderr=%q", res.Status, res.Stdout, res.Stderr)
	}
}

func TestIntegration_WorkdirWiped(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODE_RUNNER_WORKDIR", root)
	if _, err := runJob(Job{ID: "it", Language: "python", Code: "print(1)", Limits: pyLimits(10*time.Second, 1<<20)}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("workdir not wiped, leftover: %v", entries)
	}
}
