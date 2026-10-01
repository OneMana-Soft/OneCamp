package codesandbox

import (
	"strings"
	"testing"
)

func TestChartSpecJSON(t *testing.T) {
	// Valid chart object → compacted JSON.
	a := Artifact{Kind: ArtifactChart, Bytes: []byte(`{ "type":"bar", "labels":["a"] , "series":[] }`)}
	got, ok := ChartSpecJSON(a)
	if !ok {
		t.Fatal("valid chart artifact should be accepted")
	}
	if strings.ContainsAny(got, "\n") || strings.Contains(got, "  ") {
		t.Errorf("expected compact JSON, got %q", got)
	}

	// Wrong kind.
	if _, ok := ChartSpecJSON(Artifact{Kind: ArtifactFile, Bytes: []byte(`{}`)}); ok {
		t.Error("a file artifact must not be treated as a chart")
	}
	// Not JSON / not an object.
	if _, ok := ChartSpecJSON(Artifact{Kind: ArtifactChart, Bytes: []byte(`not json`)}); ok {
		t.Error("invalid JSON must be rejected")
	}
	if _, ok := ChartSpecJSON(Artifact{Kind: ArtifactChart, Bytes: []byte(`[1,2,3]`)}); ok {
		t.Error("a JSON array (not object) must be rejected")
	}
	if _, ok := ChartSpecJSON(Artifact{Kind: ArtifactChart, Bytes: []byte("   ")}); ok {
		t.Error("blank must be rejected")
	}
}

func TestSanitizeError(t *testing.T) {
	if SanitizeError("   ") != "" {
		t.Error("blank stderr → empty")
	}
	// Absolute paths are redacted.
	in := `Traceback:
  File "/work/main.py", line 3, in <module>
    open("/data/deals.csv")
ValueError: bad`
	out := SanitizeError(in)
	if strings.Contains(out, "/work") || strings.Contains(out, "/data") {
		t.Fatalf("host paths must be redacted, got %q", out)
	}
	if !strings.Contains(out, "<path>") || !strings.Contains(out, "ValueError: bad") {
		t.Fatalf("should keep the exception and redact paths, got %q", out)
	}
}

func TestSanitizeError_CapsAndKeepsTail(t *testing.T) {
	long := strings.Repeat("x", maxStderrChars+500) + "FINAL_EXCEPTION"
	out := SanitizeError(long)
	if runes := len([]rune(out)); runes > maxStderrChars+1 { // +1 for the leading ellipsis rune
		t.Fatalf("output not capped: runes=%d", runes)
	}
	if !strings.HasSuffix(out, "FINAL_EXCEPTION") {
		t.Error("must keep the tail (the actual exception)")
	}
}

func TestFailureMessage(t *testing.T) {
	if FailureMessage(Result{Status: StatusOK}) != "" {
		t.Error("ok → no failure message")
	}
	cases := map[RunStatus]string{
		StatusTimeout:     "timed out",
		StatusOOM:         "out of memory",
		StatusKilledLimit: "resource limits",
	}
	for status, want := range cases {
		if msg := FailureMessage(Result{Status: status}); !strings.Contains(msg, want) {
			t.Errorf("%q message = %q, want it to contain %q", status, msg, want)
		}
	}
	// Error with stderr includes the sanitized text.
	msg := FailureMessage(Result{Status: StatusError, Stderr: `File "/work/x.py"` + "\nKeyError: 'z'"})
	if !strings.Contains(msg, "KeyError") || strings.Contains(msg, "/work") {
		t.Errorf("error message should include sanitized stderr, got %q", msg)
	}
}
