package models

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Dgraph refuses a query that asks for the same predicate twice in one block
// ("not allowed multiple times in same sub-query"), and it refuses the whole
// query: on 6 Oct 2026 one duplicated line emptied every calendar. It compiles,
// and only a live Dgraph notices, so this reads the query text instead.
func TestNoQueryAsksForAPredicateTwiceInOneBlock(t *testing.T) {
	var files []string
	for _, root := range []string{"../../domain", "../../models/dgraph", "../../business"} {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				files = append(files, p)
			}
			return nil
		})
	}
	if len(files) < 20 {
		t.Fatalf("found only %d source files; the walk is looking in the wrong place", len(files))
	}
	checked := 0
	for _, f := range files {
		fset := token.NewFileSet()
		node, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		ast.Inspect(node, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !strings.HasPrefix(lit.Value, "`") {
				return true
			}
			q, _ := strconv.Unquote(lit.Value)
			if !strings.Contains(q, "(func:") {
				return true
			}
			checked++
			for _, dup := range duplicatePredicates(q) {
				t.Errorf("%s: %q is asked for twice in one block", fset.Position(lit.Pos()), dup)
			}
			return true
		})
	}
	if checked < 20 {
		t.Fatalf("checked only %d queries; the scan is not finding them", checked)
	}
}

var predicateLine = regexp.MustCompile(`^([A-Za-z_][\w.~]*)\s*(?::\s*[\w.~]+\s*)?(?:@.*|\(.*)?\{?\s*$`)

// duplicatePredicates returns the names that appear twice in the same block.
// An alias ("name: pred") counts by its alias, which is what Dgraph keys on.
func duplicatePredicates(q string) []string {
	var out []string
	stack := []map[string]bool{{}}
	for _, raw := range strings.Split(q, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		closes := strings.Count(line, "}")
		if line == "}" || (strings.HasPrefix(line, "}") && closes > 0) {
			for i := 0; i < closes && len(stack) > 1; i++ {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		m := predicateLine.FindStringSubmatch(line)
		if m == nil {
			// A function call, filter or variable block: only its braces matter.
			for i := 0; i < strings.Count(line, "{"); i++ {
				stack = append(stack, map[string]bool{})
			}
			for i := 0; i < closes && len(stack) > 1; i++ {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		name := m[1]
		cur := stack[len(stack)-1]
		if cur[name] && name != "uid" && name != "var" {
			out = append(out, name)
		}
		cur[name] = true
		if strings.HasSuffix(line, "{") {
			stack = append(stack, map[string]bool{})
		}
	}
	return out
}

func TestDuplicatePredicatesFindsTheCalendarBug(t *testing.T) {
	q := "{\n events(func: eq(x, 1)) {\n  event_title\n  event_is_focus\n  event_is_focus\n  creator {\n   user_name\n  }\n  other {\n   user_name\n  }\n }\n}"
	got := duplicatePredicates(q)
	if len(got) != 1 || got[0] != "event_is_focus" {
		t.Fatalf("got %v, want [event_is_focus]", got)
	}
}
