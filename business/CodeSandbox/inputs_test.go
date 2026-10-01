package codesandbox

import (
	"strings"
	"testing"
)

func TestPrepareInputFiles_SanitizesAndDedupes(t *testing.T) {
	got := PrepareInputFiles([]InputFile{
		{Name: "Q3 Deals!", Format: FormatCSV},
		{Name: "Q3 Deals!", Format: FormatCSV}, // collides after sanitizing
		{Name: "123start", Format: FormatJSON},
		{Name: "   ", Format: FormatCSV}, // empty → positional
	})
	if len(got) != 4 {
		t.Fatalf("want 4, got %d", len(got))
	}
	if got[0].Var != "Q3_Deals" {
		t.Errorf("sanitize failed: %q", got[0].Var)
	}
	if got[1].Var == got[0].Var {
		t.Errorf("collision not disambiguated: %q", got[1].Var)
	}
	if !strings.HasPrefix(got[2].Var, "_123") {
		t.Errorf("leading digit not fixed: %q", got[2].Var)
	}
	if got[3].Var != "input_4" {
		t.Errorf("empty name fallback failed: %q", got[3].Var)
	}
	// Paths carry the right extension + live under /data.
	if !strings.HasPrefix(got[0].Path, DataDir+"/") || !strings.HasSuffix(got[0].Path, ".csv") {
		t.Errorf("csv path wrong: %q", got[0].Path)
	}
	if !strings.HasSuffix(got[2].Path, ".json") {
		t.Errorf("json path wrong: %q", got[2].Path)
	}
}

func TestFilesMap(t *testing.T) {
	prepared := PrepareInputFiles([]InputFile{{Name: "deals", Format: FormatCSV, Bytes: []byte("x")}})
	m := FilesMap(prepared)
	if _, ok := m["deals.csv"]; !ok {
		t.Fatalf("expected deals.csv key, got %v", keysOf(m))
	}
}

func TestBuildPythonPreamble(t *testing.T) {
	if BuildPythonPreamble(nil) != "" {
		t.Error("no inputs → empty preamble")
	}
	prepared := PrepareInputFiles([]InputFile{
		{Name: "deals", Format: FormatCSV},
		{Name: "config", Format: FormatJSON},
	})
	p := BuildPythonPreamble(prepared)
	for _, want := range []string{
		"import pandas as _pd", // pandas with fallback
		`deals = _load_csv("data/deals.csv")`,
		`config = _load_json("data/config.json")`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("preamble missing %q\n---\n%s", want, p)
		}
	}
	// The preamble must not reference the network.
	if strings.Contains(strings.ToLower(p), "http") || strings.Contains(strings.ToLower(p), "socket") {
		t.Error("preamble must not reference the network")
	}
}

func TestEncodeCSV_RaggedRowsAligned(t *testing.T) {
	out := string(EncodeCSV(
		[]string{"a", "b", "c"},
		[][]string{
			{"1", "2", "3"},
			{"only-a"},           // short → padded
			{"w", "x", "y", "z"}, // long → truncated
			{"needs,quote", "line\nbreak", "ok"},
		},
	))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if lines[0] != "a,b,c" {
		t.Fatalf("header wrong: %q", lines[0])
	}
	// Short row padded to 3 columns.
	if !strings.HasPrefix(lines[2], "only-a,,") {
		t.Errorf("short row not padded: %q", lines[2])
	}
	// Long row truncated to 3 columns (no trailing z).
	if strings.Contains(out, ",z") {
		t.Errorf("long row not truncated: %q", out)
	}
	// Special chars are quoted (RFC 4180 via encoding/csv).
	if !strings.Contains(out, `"needs,quote"`) {
		t.Errorf("comma value not quoted: %q", out)
	}
}

func keysOf(m map[string][]byte) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	return k
}
