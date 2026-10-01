package codesandbox

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// inputs.go — pure helpers for exposing permission-checked data to a run.
//
// The orchestrator resolves each InputBinding to concrete rows/bytes (enforcing
// the caller's permissions and volume caps — that part lives with the DB layer)
// and then uses these helpers to (a) encode the data to a file placed read-only
// under /data in the sandbox, and (b) build a language preamble that loads each
// file into a conveniently-named variable. Keeping this pure makes the
// data→code contract explicit and unit-testable without a runner or DB.

// DataDir is where injected input files live, RELATIVE to the run's working
// directory (the sidecar runs with CWD = the per-run dir and writes inputs to
// ./data). Relative — not an absolute /data — so it resolves inside the sandbox's
// writable tmpfs workdir on a read-only root filesystem. Likewise, programs
// write outputs to the ./out directory (collected as artifacts).
const DataDir = "data"

// FileFormat is how an input is serialized.
type FileFormat string

const (
	FormatCSV  FileFormat = "csv"
	FormatJSON FileFormat = "json"
)

func (f FileFormat) ext() string {
	switch f {
	case FormatJSON:
		return "json"
	default:
		return "csv"
	}
}

// InputFile describes one serialized input to expose to the code.
type InputFile struct {
	// Name is the caller-chosen binding name; it is sanitized to a valid,
	// unique variable/file identifier by PrepareInputFiles.
	Name   string
	Format FileFormat
	Bytes  []byte
}

// pyIdentRE strips anything not allowed in a Python identifier.
var pyIdentRE = regexp.MustCompile(`[^A-Za-z0-9_]`)

// sanitizeIdent turns an arbitrary binding name into a safe, non-empty Python
// identifier (also used as the file stem). Leading digits are prefixed; empty
// results fall back to a positional name.
func sanitizeIdent(name string, idx int) string {
	s := pyIdentRE.ReplaceAllString(strings.TrimSpace(name), "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return fmt.Sprintf("input_%d", idx+1)
	}
	if s[0] >= '0' && s[0] <= '9' {
		s = "_" + s
	}
	return s
}

// PreparedInput is an InputFile with its resolved, unique identifier + path.
type PreparedInput struct {
	Var    string // the variable/file stem exposed to code (unique)
	Path   string // absolute path inside the sandbox, e.g. /data/deals.csv
	Format FileFormat
	Bytes  []byte
}

// PrepareInputFiles assigns each input a unique, safe identifier and path,
// deterministically (stable order by original name then index) so a run is
// reproducible. Duplicate/colliding names are disambiguated with a suffix.
func PrepareInputFiles(inputs []InputFile) []PreparedInput {
	out := make([]PreparedInput, 0, len(inputs))
	used := map[string]bool{}
	for i, in := range inputs {
		base := sanitizeIdent(in.Name, i)
		v := base
		for n := 2; used[v]; n++ {
			v = fmt.Sprintf("%s_%d", base, n)
		}
		used[v] = true
		format := in.Format
		if format == "" {
			format = FormatCSV
		}
		out = append(out, PreparedInput{
			Var:    v,
			Path:   DataDir + "/" + v + "." + format.ext(),
			Format: format,
			Bytes:  in.Bytes,
		})
	}
	return out
}

// FilesMap returns the name→bytes map for a Job.Files from prepared inputs (the
// file name is the path's basename).
func FilesMap(prepared []PreparedInput) map[string][]byte {
	m := make(map[string][]byte, len(prepared))
	for _, p := range prepared {
		base := p.Path
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		m[base] = p.Bytes
	}
	return m
}

// BuildPythonPreamble emits a preamble that loads each prepared input into a
// variable named after it: a CSV becomes a pandas DataFrame (falling back to a
// list of dict rows if pandas is unavailable), a JSON file becomes the parsed
// object. The preamble is deterministic and self-contained; it references only
// the injected /data files (never the network).
func BuildPythonPreamble(prepared []PreparedInput) string {
	if len(prepared) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# --- OneCamp sandbox preamble (auto-generated) ---\n")
	b.WriteString("import json as _json\n")
	b.WriteString("try:\n    import pandas as _pd\nexcept Exception:\n    _pd = None\n")
	b.WriteString("import csv as _csv\n")
	b.WriteString("def _load_csv(_p):\n")
	b.WriteString("    if _pd is not None:\n        return _pd.read_csv(_p)\n")
	b.WriteString("    with open(_p, newline='') as _f:\n        return list(_csv.DictReader(_f))\n")
	b.WriteString("def _load_json(_p):\n")
	b.WriteString("    with open(_p) as _f:\n        return _json.load(_f)\n")
	for _, p := range prepared {
		switch p.Format {
		case FormatJSON:
			b.WriteString(fmt.Sprintf("%s = _load_json(%q)\n", p.Var, p.Path))
		default:
			b.WriteString(fmt.Sprintf("%s = _load_csv(%q)\n", p.Var, p.Path))
		}
	}
	b.WriteString("# --- end preamble ---\n")
	return b.String()
}

// EncodeCSV serializes a header + string rows to RFC-4180 CSV bytes. Rows longer
// than the header are truncated and shorter rows padded, so a ragged input can
// never desync columns. Deterministic.
func EncodeCSV(header []string, rows [][]string) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write(header)
	width := len(header)
	for _, r := range rows {
		rec := make([]string, width)
		for i := 0; i < width; i++ {
			if i < len(r) {
				rec[i] = r[i]
			}
		}
		_ = w.Write(rec)
	}
	w.Flush()
	return buf.Bytes()
}

// SortedKeys returns the keys of m in stable order — handy for deterministic
// CSV column ordering when a caller builds rows from a map.
func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
