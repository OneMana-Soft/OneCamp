package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The trees that hold the server's graph queries.
var dgraphQueryRoots = []string{"../domain", "../models", "../business", "../controllers", "../services"}

// dgraphQueries is every graph query in the server's source, for the guards
// that read them (TestNoDgraphQueryRepeatsAFieldInOneBlock,
// TestNoDgraphQueryAsksForAPredicateNothingHolds): one walk, so the guards
// read the same queries the same way.
func dgraphQueries(t *testing.T) []dgraphQuery {
	t.Helper()
	var out []dgraphQuery
	for _, root := range dgraphQueryRoots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			out = append(out, dgraphQueriesIn(t, path)...)
			return nil
		})
	}
	if len(out) < 250 {
		t.Fatalf("read %d graph queries; the scan has stopped seeing them", len(out))
	}
	return out
}

// dgraphQuery is one graph query in the server's source: the file it is in
// (from the repository's root), the line it starts on, and its text.
type dgraphQuery struct {
	file string
	line int
	text string
}

// dgraphQueriesIn is each graph query in a Go file: a string literal that
// asks for (func:, or a sum of strings with one, read whole with each part
// that isn't a literal as a space. A literal inside a call in the sum
// (fmt.Sprintf(`...`)) is a query of its own. Quoted literals count as well
// as raw ones: a query built a block at a time writes its blocks in them
// ("ch%d as var(func: eq(chat_uuid, %q))\n"), and those blocks went unread.
func dgraphQueriesIn(t *testing.T, path string) []dgraphQuery {
	t.Helper()
	file := strings.TrimPrefix(filepath.ToSlash(path), "../")
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []dgraphQuery
	inSum := map[ast.Node]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		if n == nil || inSum[n] {
			return true
		}
		switch e := n.(type) {
		case *ast.BinaryExpr:
			if e.Op != token.ADD {
				return true
			}
			var b strings.Builder
			var walk func(ast.Expr)
			walk = func(x ast.Expr) {
				switch v := x.(type) {
				case *ast.BinaryExpr:
					if v.Op == token.ADD {
						inSum[v] = true
						walk(v.X)
						walk(v.Y)
						return
					}
				case *ast.ParenExpr:
					inSum[v] = true
					walk(v.X)
					return
				case *ast.BasicLit:
					if s, err := strconv.Unquote(v.Value); err == nil && v.Kind == token.STRING {
						inSum[v] = true
						b.WriteString(s)
						return
					}
				}
				b.WriteString(" ")
			}
			walk(e)
			if text := b.String(); strings.Contains(text, "(func:") {
				out = append(out, dgraphQuery{file: file, line: fset.Position(e.Pos()).Line, text: text})
			}
		case *ast.BasicLit:
			if e.Kind != token.STRING {
				return true
			}
			if text, err := strconv.Unquote(e.Value); err == nil && strings.Contains(text, "(func:") {
				out = append(out, dgraphQuery{file: file, line: fset.Position(e.Pos()).Line, text: text})
			}
		}
		return true
	})
	return out
}

// A query built from pieces is read whole, so a value variable one piece
// defines is known where another piece uses it.
func TestDgraphQueriesInReadsAQueryBuiltFromPiecesWhole(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\nimport \"fmt\"\n\nconst top = `not has(task_parent_task)`\n\n" +
		"var q = `query Q($u: string) { var(func: uid($u)) { s as task_start_date } ` + top + ` me(func: uid($u)) { first: min(val(s)) } }`\n\n" +
		"var r = \"# \" + fmt.Sprintf(`{ n(func: eq(ch_uuid, %q)) { ch_name } }`, \"x\")\n" +
		"var d = fmt.Sprintf(\"  ch%d as var(func: eq(ch_handle, %q))\\n\", 1, \"x\")\n"
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found := dgraphQueriesIn(t, path)
	if len(found) != 3 || !strings.Contains(found[0].text, "s as task_start_date") || !strings.Contains(found[0].text, "min(val(s))") ||
		!strings.HasPrefix(found[1].text, "{ n(func:") || !strings.Contains(found[2].text, "eq(ch_handle,") {
		t.Fatalf("read %+v, want the query built from pieces whole, the one in a call in a sum, and the quoted block", found)
	}
}
