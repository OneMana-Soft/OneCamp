package codesandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func baseParams(r Runner) RunParams {
	return RunParams{
		JobID:    "j1",
		Language: LanguagePython,
		Code:     "print(1)",
		Enabled:  true,
		Runner:   r,
	}
}

func TestRun_RefusesWhenDisabled(t *testing.T) {
	out := Run(context.Background(), RunParams{Language: LanguagePython, Code: "x", Enabled: false, Runner: &MockRunner{}})
	if !out.Refused || out.Reason != StopReasonDisabled {
		t.Fatalf("disabled → refused sandbox_disabled, got %+v", out)
	}
	if out.CodeSHA256 == "" {
		t.Error("code hash should be set even on refusal (for audit)")
	}
}

func TestRun_RefusesUnsupportedLanguage(t *testing.T) {
	out := Run(context.Background(), RunParams{Language: "bash", Code: "x", Enabled: true, Runner: &MockRunner{}})
	if !out.Refused || out.Reason != StopReasonUnavailable {
		t.Fatalf("unsupported language → refused, got %+v", out)
	}
}

func TestRun_RefusesOverBudget(t *testing.T) {
	p := baseParams(&MockRunner{})
	p.EstSeconds = 100
	p.Tiers = []Tier{{Reason: StopReasonWorkspaceBudget, Caps: Caps{DailySeconds: 60}, Usage: TierUsage{Seconds: 55}}}
	out := Run(context.Background(), p)
	if !out.Refused || out.Reason != StopReasonWorkspaceBudget {
		t.Fatalf("over budget → refused workspace, got %+v", out)
	}
	if (&MockRunner{}).Calls != 0 {
		t.Error("runner must not be called when refused")
	}
}

func TestRun_NilRunnerUnavailable(t *testing.T) {
	p := baseParams(nil)
	out := Run(context.Background(), p)
	if !out.Refused || out.Reason != StopReasonUnavailable {
		t.Fatalf("nil runner → unavailable, got %+v", out)
	}
}

func TestRun_RunnerErrorUnavailable(t *testing.T) {
	p := baseParams(&MockRunner{Err: errors.New("down")})
	out := Run(context.Background(), p)
	if !out.Refused || out.Reason != StopReasonUnavailable {
		t.Fatalf("runner error → unavailable, got %+v", out)
	}
}

func TestRun_InjectsPreambleAndFiles(t *testing.T) {
	m := &MockRunner{Result: Result{Status: StatusOK, Stdout: "done"}}
	p := baseParams(m)
	p.Inputs = []InputFile{{Name: "deals", Format: FormatCSV, Bytes: []byte("a,b\n1,2\n")}}
	_ = Run(context.Background(), p)

	if m.Calls != 1 {
		t.Fatalf("runner should be called once, got %d", m.Calls)
	}
	// The job code must carry the preamble (loading the injected file) then the
	// user's code.
	if !strings.Contains(m.LastJob.Code, `deals = _load_csv("data/deals.csv")`) {
		t.Errorf("job code missing preamble loader:\n%s", m.LastJob.Code)
	}
	if !strings.Contains(m.LastJob.Code, "print(1)") {
		t.Errorf("job code missing user code:\n%s", m.LastJob.Code)
	}
	if _, ok := m.LastJob.Files["deals.csv"]; !ok {
		t.Errorf("injected file missing from job: %v", m.LastJob.Files)
	}
	// Limits must be clamped (defaults filled).
	if m.LastJob.Limits.Wall <= 0 {
		t.Error("limits not clamped")
	}
}

func TestRun_ClassifiesChartsFilesAndStdout(t *testing.T) {
	m := &MockRunner{Result: Result{
		Status: StatusOK,
		Stdout: "  summary  ",
		Artifacts: []Artifact{
			{Kind: ArtifactChart, Bytes: []byte(`{"type":"bar","labels":["a"],"series":[]}`)},
			{Kind: ArtifactChart, Bytes: []byte(`not json`)}, // invalid → becomes a file
			{Kind: ArtifactFile, Name: "out.csv", ContentType: "text/csv", Bytes: []byte("x")},
		},
	}}
	out := Run(context.Background(), baseParams(m))
	if out.Refused || out.Status != StatusOK {
		t.Fatalf("expected OK, got %+v", out)
	}
	if len(out.Charts) != 1 {
		t.Errorf("want 1 valid chart, got %d", len(out.Charts))
	}
	if len(out.Files) != 2 { // the invalid chart + the real file
		t.Errorf("want 2 files (invalid chart demoted + real file), got %d", len(out.Files))
	}
	if out.Stdout != "summary" {
		t.Errorf("stdout should be trimmed, got %q", out.Stdout)
	}
}

func TestRun_FailedRunCarriesMessage(t *testing.T) {
	m := &MockRunner{Result: Result{Status: StatusTimeout}}
	out := Run(context.Background(), baseParams(m))
	if out.Refused {
		t.Fatal("a run that executed but failed is not a refusal")
	}
	if out.Status != StatusTimeout || !strings.Contains(out.Message, "timed out") {
		t.Fatalf("expected a timeout message, got %+v", out)
	}
}

func TestRun_CodeHashIsOfUserCodeNotPreamble(t *testing.T) {
	m := &MockRunner{Result: Result{Status: StatusOK}}
	p1 := baseParams(m)
	p1.Code = "print(1)"
	p1.Inputs = []InputFile{{Name: "x", Format: FormatCSV, Bytes: []byte("a\n")}}
	h1 := Run(context.Background(), p1).CodeSHA256

	p2 := baseParams(m)
	p2.Code = "print(1)" // same user code, no inputs → different preamble
	h2 := Run(context.Background(), p2).CodeSHA256

	if h1 != h2 {
		t.Errorf("hash must be of the user code only, independent of injected preamble: %s vs %s", h1, h2)
	}
}
