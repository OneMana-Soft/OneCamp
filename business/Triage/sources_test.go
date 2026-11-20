package business

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Every door's source is one the table takes (migration 595's check on
// triage_items.source, varchar(8)), and the table takes no source no door
// uses: the work that arrives from a form, the API, an agent, GitHub, a chat
// or an import (linear-21 to_win 1).
func TestEverySourceIsOneTheTableTakes(t *testing.T) {
	// The newest triage migration that sets the check is the table's rule
	// (595 made it; the mail door's migration widened it).
	files, err := filepath.Glob("../../migrations/*_triage*.up.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("the triage migrations: %v %v", files, err)
	}
	slices.SortFunc(files, func(a, b string) int { return migrationNumber(a) - migrationNumber(b) })
	var m [][]byte
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if got := regexp.MustCompile(`source IN \(([^)]*)\)`).FindSubmatch(raw); got != nil {
			m = got
		}
	}
	if m == nil {
		t.Fatal("no triage migration checks the source")
	}
	var took []string
	for _, q := range strings.Split(string(m[1]), ",") {
		took = append(took, strings.Trim(strings.TrimSpace(q), "'"))
	}
	var ours []string
	for _, s := range Sources {
		if len(s) > 8 {
			t.Errorf("source %q is longer than the column's 8", s)
		}
		ours = append(ours, string(s))
	}
	slices.Sort(took)
	slices.Sort(ours)
	if !slices.Equal(took, ours) {
		t.Errorf("the table takes %v, the doors use %v", took, ours)
	}
	want := []string{"agent", "api", "chat", "form", "github", "import", "mail"}
	if !slices.Equal(ours, want) {
		t.Errorf("sources %v, want %v", ours, want)
	}
}

// migrationNumber is a migration file's number: 595 for .../595_triage.up.sql.
func migrationNumber(path string) int {
	n, _ := strconv.Atoi(strings.SplitN(filepath.Base(path), "_", 2)[0])
	return n
}
