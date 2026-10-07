package business

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The built-in templates as JSON, for the template pages on onemana.dev, which
// show exactly what a project made from each one gets. Runs only when asked,
// from the repository root:
//
//	TEMPLATES_EXPORT="$PWD/../onemana-frontend/content/templates.json" go test -count=1 ./business/ProjectTemplate/ -run TestExportBuiltIns
//
// The path must be absolute, since go test runs in the package's directory,
// and -count=1 matters: a cached pass writes nothing.
func TestExportBuiltIns(t *testing.T) {
	path := os.Getenv("TEMPLATES_EXPORT")
	if path == "" {
		t.Skip("set TEMPLATES_EXPORT to write the built-in templates")
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("TEMPLATES_EXPORT must be an absolute path (go test runs in %s): %q", "business/ProjectTemplate", path)
	}
	out := make([]Template, 0, len(builtins))
	for _, b := range builtins {
		checked, err := Check(b)
		if err != nil {
			t.Fatalf("%s: %v", b.ID, err)
		}
		checked.ID = b.ID
		out = append(out, checked)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // descriptions are HTML; keep them readable
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
