package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// handExecutorBaseline is how many functions in business/, domain/ and
// models/ still run SQL or DQL themselves rather than through models/pgexec
// or models/dgexec (1,219 when this landed; the audit, counting differently,
// said about 1,090; 1,170 on beta 23c884e2, and 1,168 once the projects chain
// landed on it, the two its branch had moved onto the generics). It only goes
// down: move a function onto the generics, then lower this to the count the
// test reports. The release lines leave out the demo's own code, so theirs is
// one lower (1,166 on v2.75.0's main).
const handExecutorBaseline = 1165

// backgroundTimeoutBaseline is how many times models/ starts a statement's
// timeout from context.Background() instead of the caller's ctx (206 at the
// audit, 199 when this landed). It only goes down.
const backgroundTimeoutBaseline = 151

// executorCalls are the calls that run a statement or a query on a store
// client, or open a graph transaction to run one in (an HTTP client's Do is
// not one, so a graph write is counted by its NewTxn).
var executorCalls = map[string]bool{
	"ExecContext": true, "QueryContext": true, "QueryRowContext": true, "BeginTx": true,
	"QueryWithVars": true, "NewTxn": true, "NewReadOnlyTxn": true,
}

// goFuncs walks the non-test Go files under roots and calls fn with each
// function declaration, skipping dirs whose slash path ends in one of skip.
func goFuncs(t *testing.T, roots, skip []string, fn func(path string, fd *ast.FuncDecl)) int {
	t.Helper()
	files := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				for _, s := range skip {
					if strings.HasSuffix(filepath.ToSlash(path), s) {
						return filepath.SkipDir
					}
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			files++
			for _, decl := range f.Decls {
				if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil {
					fn(path, fd)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

// calledMethods is the method names fd's body calls (x.Name(...)).
func calledMethods(fd *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if s, ok := c.Fun.(*ast.SelectorExpr); ok {
				out[s.Sel.Name] = true
			}
		}
		return true
	})
	return out
}

// Hand-written executors only get fewer.
//
// WHY. The audit counted about 1,090 functions that each run SQL or DQL their
// own way: their own timeout (or none), their own rows.Err() (or none), their
// own transaction (read-write for a read, never discarded). models/pgexec and
// models/dgexec run a statement one way. This counts the functions in
// business/, domain/ and models/ that still call a store client themselves,
// and fails when there are more than handExecutorBaseline, or fewer (so the
// baseline is lowered with the change that moved them).
func TestHandExecutorsOnlyGetFewer(t *testing.T) {
	count := 0
	files := goFuncs(t, []string{"../business", "../domain", "../models"}, []string{"models/pgexec", "models/dgexec"}, func(_ string, fd *ast.FuncDecl) {
		for name := range calledMethods(fd) {
			if executorCalls[name] {
				count++
				return
			}
		}
	})
	if files < 1000 {
		t.Fatalf("read %d files; the guard is no longer looking at the executors", files)
	}
	switch {
	case count > handExecutorBaseline:
		t.Errorf("%d functions run SQL or DQL by hand, more than the %d allowed: run the new one through models/pgexec or models/dgexec", count, handExecutorBaseline)
	case count < handExecutorBaseline:
		t.Errorf("%d functions run SQL or DQL by hand now, fewer than %d: lower handExecutorBaseline to %d", count, handExecutorBaseline, count)
	}
}

// A model's read never leaves its transaction open.
//
// WHY. 32 read executors in models/ opened a read-write transaction
// (NewTxn) for a query and never discarded it: a read-write transaction for a
// read, and one Dgraph keeps until it times out. A read is
// NewReadOnlyTxn with defer Discard, or models/dgexec.Read. This fails on a
// function in models/ that opens NewTxn, queries, writes nothing and never
// calls Discard. It baselines nothing.
func TestModelReadsDiscardTheirTransactions(t *testing.T) {
	goFuncs(t, []string{"../models"}, nil, func(path string, fd *ast.FuncDecl) {
		m := calledMethods(fd)
		read := m["QueryWithVars"] || m["Query"]
		write := m["Mutate"] || m["Do"] || m["Commit"]
		if m["NewTxn"] && read && !write && !m["Discard"] {
			t.Errorf("%s: %s opens a read-write transaction for a read and never discards it; use NewReadOnlyTxn with defer Discard, or models/dgexec", path, fd.Name.Name)
		}
	})
}

// Models start fewer statement timeouts from context.Background().
//
// WHY. A model that times its statement from context.Background() keeps
// running it after the request that asked has gone, and drops the request's
// values (its id, its user) from the logs. The audit counted 206 in models/.
// models/pgexec times a statement from the caller's ctx. This counts them and
// fails when there are more than backgroundTimeoutBaseline, or fewer (lower it).
func TestModelsTimeFewerStatementsFromBackground(t *testing.T) {
	count := 0
	err := filepath.WalkDir("../models", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		count += strings.Count(string(b), "context.WithTimeout(context.Background()")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case count > backgroundTimeoutBaseline:
		t.Errorf("models/ times %d statements from context.Background(), more than %d: take the caller's ctx (models/pgexec does)", count, backgroundTimeoutBaseline)
	case count < backgroundTimeoutBaseline:
		t.Errorf("models/ times %d statements from context.Background() now, fewer than %d: lower backgroundTimeoutBaseline to %d", count, backgroundTimeoutBaseline, count)
	}
}
