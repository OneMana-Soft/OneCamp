package pgexec

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// madeByItsCaller is each function whose raw statement on a transaction is
// text its caller hands in, and why that text never only reads.
var madeByItsCaller = map[string]string{
	"collectCells": "its callers' statements rewrite a field's cells (setCellsSQL, cellsChangedOf: UPDATE ... RETURNING)",
	"fieldCells":   "on a transaction it reads FOR UPDATE (UpdateFieldAndCells); FieldCells reads on the pool",
}

// A read on a transaction is made through pgexec, never raw (V-611, CR-226
// condition 3). In a transaction that ran WithoutWorkers pgexec plans a read
// for its run (plannedForItsRun); a raw read runs the plan its session
// prepared, a Gather included when the session prepared it with workers
// (outside such a transaction: the same text on the pool), and its workers
// run out of shared memory in a write that is never run again. The ledger's
// transactions are models/postgres/DataTable's (ledgerTx), and their
// observers (business/RowQuery, business/DataTable) read in them: there a raw
// statement on anything but the pool only writes or locks rows, which
// Postgres never plans with workers.
func TestARawStatementOnATransactionNeverOnlyReads(t *testing.T) {
	seen := map[string]bool{}
	raw := 0
	for _, dir := range []string{"../postgres/DataTable", "../../business/RowQuery", "../../business/DataTable"} {
		fset := token.NewFileSet()
		var files []*ast.File
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		var all []ast.Node
		for _, f := range files {
			all = append(all, f)
		}
		pkg := definitions(all...)
		for _, f := range files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				r := sqlText{pkg: pkg, local: definitions(fn)}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok || len(call.Args) < 2 {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || (sel.Sel.Name != "QueryContext" && sel.Sel.Name != "QueryRowContext" && sel.Sel.Name != "ExecContext") || r.onThePool(sel.X) {
						return true
					}
					raw++
					at := fset.Position(call.Pos())
					q, ok := r.text(call.Args[1], 0)
					switch {
					case !ok && madeByItsCaller[fn.Name.Name] != "":
						seen[fn.Name.Name] = true
					case !ok:
						t.Errorf("%s:%d %s: a raw statement on a transaction whose text this test can't read; make it through pgexec, or name %s in madeByItsCaller with why it never only reads", filepath.Base(at.Filename), at.Line, fn.Name.Name, fn.Name.Name)
					case readOnly(q):
						t.Errorf("%s:%d %s reads raw on a transaction (%.70q): make it through pgexec (pgexec.WithTx(ctx, tx)), which plans it for its run where WithoutWorkers holds", filepath.Base(at.Filename), at.Line, fn.Name.Name, q)
					}
					return true
				})
			}
		}
	}
	for name := range madeByItsCaller {
		if !seen[name] {
			t.Errorf("madeByItsCaller names %s, which makes no raw statement on a transaction now: take it out", name)
		}
	}
	if raw < 20 {
		t.Errorf("found %d raw statements on a transaction; want the ledger's (20 or more): is the scan still reading the right files?", raw)
	}
}

// definitions is each name's defining expressions under n: a const or var
// and its value, an assignment's left side and its right, a parameter (no
// expression, so its text is the caller's).
func definitions(nodes ...ast.Node) map[string][]ast.Expr {
	out := map[string][]ast.Expr{}
	for _, n := range nodes {
		ast.Inspect(n, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ValueSpec:
				for i, name := range n.Names {
					if i < len(n.Values) {
						out[name.Name] = append(out[name.Name], n.Values[i])
					} else {
						out[name.Name] = append(out[name.Name], nil)
					}
				}
			case *ast.AssignStmt:
				for i, l := range n.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						if len(n.Lhs) == len(n.Rhs) && n.Tok != token.ADD_ASSIGN {
							out[id.Name] = append(out[id.Name], n.Rhs[i])
						} else {
							out[id.Name] = append(out[id.Name], nil)
						}
					}
				}
			case *ast.Field:
				for _, name := range n.Names {
					out[name.Name] = append(out[name.Name], nil)
				}
			}
			return true
		})
	}
	return out
}

// sqlText reads a statement's text from a package's source.
type sqlText struct{ pkg, local map[string][]ast.Expr }

// one is name's one defining expression, local first; ok false when it has
// none, or more than one.
func (s sqlText) one(name string) (ast.Expr, bool) {
	defs, ok := s.local[name]
	if !ok {
		defs = s.pkg[name]
	}
	if len(defs) != 1 || defs[0] == nil {
		return nil, false
	}
	return defs[0], true
}

// onThePool is whether x is the pool (postgresInit.DBConn.SqlDB, or a name
// for it), where no transaction, and so no WithoutWorkers, holds.
func (s sqlText) onThePool(x ast.Expr) bool {
	if id, ok := x.(*ast.Ident); ok {
		if e, ok := s.one(id.Name); ok {
			x = e
		}
	}
	return types.ExprString(x) == "postgresInit.DBConn.SqlDB"
}

// text is e's text: literals, constants, names given one value, + and
// fmt.Sprintf of those. ok false when part of it is the caller's.
func (s sqlText) text(e ast.Expr, depth int) (string, bool) {
	if depth > 16 {
		return "", false
	}
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			v, err := strconv.Unquote(e.Value)
			return v, err == nil
		}
		return e.Value, true
	case *ast.ParenExpr:
		return s.text(e.X, depth+1)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		x, ok := s.text(e.X, depth+1)
		y, ok2 := s.text(e.Y, depth+1)
		return x + y, ok && ok2
	case *ast.Ident:
		def, ok := s.one(e.Name)
		if !ok {
			return "", false
		}
		return s.text(def, depth+1)
	case *ast.CallExpr:
		if types.ExprString(e.Fun) != "fmt.Sprintf" || len(e.Args) == 0 {
			return "", false
		}
		format, ok := s.text(e.Args[0], depth+1)
		args := make([]any, 0, len(e.Args)-1)
		for _, a := range e.Args[1:] {
			v, ok2 := s.text(a, depth+1)
			ok = ok && ok2
			args = append(args, v)
		}
		return fmt.Sprintf(format, args...), ok
	}
	return "", false
}
