package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// pageSizeNames are the query parameters a list's page size goes by.
var pageSizeNames = map[string]bool{"limit": true, "pageSize": true, "page_size": true, "per_page": true, "perPage": true, "size": true}

// A handler reads a list's page size through SizeParam (or LimitParam, or
// ListPaging for a list paged by pageSize and pageIndex), never by hand.
//
// WHY. Forty-three handlers read their page size by hand, each its own way:
// some kept a size past their most as the default, some took any number, so
// the activity mentions, a channel list or the admins could be asked for a
// million rows at once, and a negative size went to the graph as it came. A
// handler written later would read its own unless something stops it. This
// guard reads every handler: a page size's parameter (pageSizeNames) read with
// Get, or by index, is a page size read by hand. It baselines nothing.
func TestHandlersReadPageSizesThroughSizeParam(t *testing.T) {
	read := 0
	err := filepath.WalkDir("../controllers", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		name := func(e ast.Expr) (string, bool) {
			lit, ok := e.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return "", false
			}
			s, err := strconv.Unquote(lit.Value)
			return s, err == nil && pageSizeNames[s]
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				// q.Get("pageSize"), r.URL.Query().Get("limit"), r.FormValue("limit")
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok || len(n.Args) != 1 || (sel.Sel.Name != "Get" && sel.Sel.Name != "FormValue") {
					return true
				}
				if s, ok := name(n.Args[0]); ok {
					t.Errorf("%s: reads the page size %q by hand; read it with helpers.SizeParam", fset.Position(n.Pos()), s)
				}
			case *ast.IndexExpr:
				// r.URL.Query()["pageSize"]
				if s, ok := name(n.Index); ok {
					t.Errorf("%s: reads the page size %q by hand; read it with helpers.SizeParam", fset.Position(n.Pos()), s)
				}
			}
			return true
		})
		read++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Fewer files than this means the walk stopped seeing the handlers.
	if read < 100 {
		t.Fatalf("read %d handler files; the guard is no longer looking at them", read)
	}
}
