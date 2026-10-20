package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// dgraphTxnHomes are the only packages that may open a read-write graph
// transaction or commit a request themselves: dgraphInit's DoCommitNow and
// InTxn, and models/dgexec, which writes through DoCommitNow.
var dgraphTxnHomes = []string{"initializers/dgraphInit", "models/dgexec"}

// Every graph write retries an abort.
//
// WHY. Dgraph aborts one of two transactions that write the same node at once
// and says "Please retry". A request sent once with a bare txn.Do (CommitNow)
// or a txn.Mutate then Commit gave up on that abort, so one of two people's
// changes was lost: of eight posts sent into one channel at once, one landed
// (chat, 19 Oct 2026), and docs, boards, projects, events, transcripts and
// thread tags were the same (tests/integration/dgraph_hot_nodes_test.go). A
// write goes through dgraphInit.DoCommitNow (one request) or dgraphInit.InTxn
// (a read, then a write decided on it), or models/dgexec.Mutate; each runs an
// aborted write again in a fresh transaction.
//
// This fails on any non-test Go file outside dgraphTxnHomes (and tests/,
// which is test support) that opens a read-write transaction (NewTxn) or
// sets CommitNow to true. A read takes
// NewReadOnlyTxn, dgraphInit.ReadInto or InSnapshot, or models/dgexec.Read.
// It baselines nothing.
func TestDgraphWritesRetryAnAbort(t *testing.T) {
	files := 0
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		slash := filepath.ToSlash(path)
		if d != nil && d.IsDir() {
			switch {
			case slash == "../data", slash == "../.git", strings.HasSuffix(slash, "/node_modules"), strings.HasSuffix(slash, "/vendor"):
				return filepath.SkipDir
			case slash == "../tests":
				// Test support only: the harness seeds a graph before anything
				// contends (it writes through DoCommitNow all the same), and
				// tests/integration/dgfixture writes through a client of its
				// own and retries an abort itself.
				return filepath.SkipDir
			}
			for _, home := range dgraphTxnHomes {
				if slash == "../"+home {
					return filepath.SkipDir
				}
			}
		}
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		isTrue := func(e ast.Expr) bool {
			id, ok := e.(*ast.Ident)
			return ok && id.Name == "true"
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if s, ok := n.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "NewTxn" {
					t.Errorf("%s: opens a read-write graph transaction by hand, so an abort is never retried: write through dgraphInit.DoCommitNow, dgraphInit.InTxn or models/dgexec.Mutate, and read with NewReadOnlyTxn", fset.Position(n.Pos()))
				}
			case *ast.KeyValueExpr:
				if k, ok := n.Key.(*ast.Ident); ok && k.Name == "CommitNow" && isTrue(n.Value) {
					t.Errorf("%s: sets CommitNow by hand, so an abort is never retried: send the request through dgraphInit.DoCommitNow (or models/dgexec.Mutate)", fset.Position(n.Pos()))
				}
			case *ast.AssignStmt:
				for i, l := range n.Lhs {
					if s, ok := l.(*ast.SelectorExpr); ok && s.Sel.Name == "CommitNow" && i < len(n.Rhs) && isTrue(n.Rhs[i]) {
						t.Errorf("%s: sets CommitNow by hand, so an abort is never retried: send the request through dgraphInit.DoCommitNow (or models/dgexec.Mutate)", fset.Position(n.Pos()))
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 1000 {
		t.Fatalf("read %d files; the guard is no longer looking at the code", files)
	}
}
