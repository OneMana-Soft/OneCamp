package helpers

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// An INSERT's placeholders must match the values bound to it.
//
// WHY THIS IS A TEST. Adding a column to a run row meant adding $5 to the VALUES
// list and a fifth argument to ExecContext. The edit that added the argument
// silently did nothing; the one that added the placeholder worked. The package
// compiled, every test passed, and the statement would have failed at runtime on
// the first agent run, which is the one place nothing was watching.
//
// Go cannot type-check this: ExecContext takes ...interface{}, so the arity is
// invisible to the compiler by design. A scan is the only thing that sees it.
var (
	valuesList  = regexp.MustCompile(`VALUES\s*\(([^)]*)\)`)
	placeholder = regexp.MustCompile(`\$(\d+)`)
)

// highestPlaceholder returns the largest $n in a VALUES clause.
func highestPlaceholder(values string) int {
	high := 0
	for _, m := range placeholder.FindAllStringSubmatch(values, -1) {
		n := 0
		for _, c := range m[1] {
			n = n*10 + int(c-'0')
		}
		if n > high {
			high = n
		}
	}
	return high
}

func TestInsertPlaceholdersAreContiguousFromOne(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	// The model layer is where the statements live.
	modelRoot := filepath.Join(root, "models", "postgres")
	err = filepath.Walk(modelRoot, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return werr
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)

		for _, m := range valuesList.FindAllStringSubmatch(string(body), -1) {
			values := m[1]
			high := highestPlaceholder(values)
			if high == 0 {
				continue // a literal VALUES list, not a parameterised one
			}
			// Every number from 1 to the highest must be present. A gap means a
			// placeholder was added or removed without renumbering, which is the
			// same class of edit as the one that motivated this.
			seen := map[int]bool{}
			for _, p := range placeholder.FindAllStringSubmatch(values, -1) {
				n := 0
				for _, c := range p[1] {
					n = n*10 + int(c-'0')
				}
				seen[n] = true
			}
			for i := 1; i <= high; i++ {
				if !seen[i] {
					t.Errorf("%s: VALUES (%s) skips $%d, so the arguments after it bind to the wrong columns",
						rel, strings.TrimSpace(values), i)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
