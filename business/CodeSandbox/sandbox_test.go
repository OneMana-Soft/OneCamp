package codesandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestValidLanguage(t *testing.T) {
	if !ValidLanguage(LanguagePython) {
		t.Error("python must be valid")
	}
	if ValidLanguage("bash") || ValidLanguage("") {
		t.Error("unknown language must be invalid")
	}
}

func TestDefaultLimits(t *testing.T) {
	d := DefaultLimits()
	if d.Wall <= 0 || d.CPU <= 0 || d.MemoryBytes <= 0 || d.PIDs <= 0 ||
		d.OutputBytes <= 0 || d.MaxArtifacts <= 0 || d.ArtifactBytes <= 0 {
		t.Fatalf("default limits must all be positive: %+v", d)
	}
}

func TestClampLimits_FillsZeros(t *testing.T) {
	got := ClampLimits(Limits{}) // all zero → all defaults
	if got != DefaultLimits() {
		t.Fatalf("zero limits should fill from defaults, got %+v", got)
	}
}

func TestClampLimits_CapsCeilings(t *testing.T) {
	huge := Limits{
		Wall:          time.Hour,
		CPU:           time.Hour,
		MemoryBytes:   1 << 40, // 1 TiB
		PIDs:          1_000_000,
		OutputBytes:   1 << 40,
		MaxArtifacts:  10_000,
		ArtifactBytes: 1 << 40,
	}
	got := ClampLimits(huge)
	if got.Wall != maxWall {
		t.Errorf("Wall not capped: %v", got.Wall)
	}
	if got.CPU != maxCPU {
		t.Errorf("CPU not capped: %v", got.CPU)
	}
	if got.MemoryBytes != maxMemoryBytes {
		t.Errorf("MemoryBytes not capped: %d", got.MemoryBytes)
	}
	if got.PIDs != maxPIDs {
		t.Errorf("PIDs not capped: %d", got.PIDs)
	}
	if got.OutputBytes != maxOutputBytes {
		t.Errorf("OutputBytes not capped: %d", got.OutputBytes)
	}
	if got.MaxArtifacts != maxMaxArtifacts {
		t.Errorf("MaxArtifacts not capped: %d", got.MaxArtifacts)
	}
	if got.ArtifactBytes != maxArtifactBytes {
		t.Errorf("ArtifactBytes not capped: %d", got.ArtifactBytes)
	}
}

func TestClampLimits_KeepsValidLowerValues(t *testing.T) {
	in := Limits{
		Wall:          5 * time.Second,
		CPU:           3 * time.Second,
		MemoryBytes:   128 << 20,
		PIDs:          16,
		OutputBytes:   1 << 20,
		MaxArtifacts:  2,
		ArtifactBytes: 512 << 10,
	}
	if got := ClampLimits(in); got != in {
		t.Fatalf("valid sub-ceiling limits must be preserved: got %+v want %+v", got, in)
	}
}

func TestRunStatusSucceeded(t *testing.T) {
	if !StatusOK.Succeeded() {
		t.Error("ok must be a success")
	}
	for _, s := range []RunStatus{StatusError, StatusTimeout, StatusKilledLimit, StatusOOM} {
		if s.Succeeded() {
			t.Errorf("%q must not be a success", s)
		}
	}
}

func TestMockRunner(t *testing.T) {
	want := Result{Status: StatusOK, Stdout: "hi", Usage: Usage{WallMS: 12}}
	m := &MockRunner{Result: want}
	got, err := m.Run(context.Background(), Job{ID: "j1", Language: LanguagePython, Code: "print('hi')"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusOK || got.Stdout != "hi" {
		t.Fatalf("unexpected result: %+v", got)
	}
	if m.Calls != 1 || m.LastJob.ID != "j1" {
		t.Fatalf("mock did not record the job: calls=%d last=%+v", m.Calls, m.LastJob)
	}

	// Error path: runner unreachable.
	boom := errors.New("runner down")
	me := &MockRunner{Err: boom}
	if _, err := me.Run(context.Background(), Job{}); !errors.Is(err, boom) {
		t.Fatalf("expected the preset error, got %v", err)
	}
}
