package business

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// Every write to a table's rows loads the table through loadEditable, so a
// member who may only view it is refused (V-170). Before V-170 every row write
// asked loadViewable, and whoever could open a table could change it.
//
// loadViewable answers only "may they open it". A function that calls it must
// be a read, listed here with why; a function listed as a write must call
// loadEditable and never loadViewable. The row ledger's writes (the batch,
// moves, undo, redo and the trash) are this package's Batch, MoveRows, Undo,
// Redo and RestoreFromTrash, listed below. A new function that loads a table
// fails here until it is listed as one or the other.
var tableReads = map[string]string{
	"GetBundle":        "opening a table",
	"ListRows":         "reading its rows",
	"RowHistory":       "a row's history",
	"TableTrash":       "the trash's list",
	"PickRows":         "choosing rows to link",
	"ListTableFields":  "its fields, for a link",
	"RowsByID":         "rows a link names",
	"PrepareExport":    "an export, which reads",
	"SetPresence":      "who is on the table, which changes no row",
	"tableForQuery":    "totals and row queries",
	"keyedRows.Window": "the API's pages of rows",
	"CanView":          "the open check itself",
	"CanManage":        "manage asks loadManageable's rule on what loadViewable loads",
	"viewToChange":     "a person's own view; a shared view asks loadManageable",
	"restoreViews":     "views back from the trash by viewToChange's rule (changeable) on the table it loads",
	"serveViewsBatch":  "the API's view writes, each by viewToChange's rule (changeable) on the table it loads",
	"loadEditable":     "the write check starts from the open check",
}

// tableWrites are the row writes; each must call loadEditable, or
// loadEditables for many tables at once.
var tableWrites = []string{
	"CreateRow", "UpdateRow", "PatchRow", "DeleteRow",
	"Batch", "MoveRows", "Undo", "Redo", "RestoreFromTrash",
	"ChangeLinks", "Transform",
	"targets", // a sheet brought into a table that's there
}

// funcName is a declaration's name as the lists write it: Name, or
// Receiver.Name for a method.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// callsOf is the plain function names fn's body calls.
func callsOf(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	if fn.Body == nil {
		return out
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}

func TestEveryRowWriteLoadsTheTableThroughLoadEditable(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]map[string]bool{}
	parsed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok {
				calls[funcName(fn)] = callsOf(fn)
			}
		}
	}
	if parsed < 20 {
		t.Fatalf("parsed %d files: the guard is looking in the wrong place", parsed)
	}

	var unlisted []string
	for name, c := range calls {
		if c["loadViewable"] && name != "loadViewable" {
			if _, read := tableReads[name]; !read {
				unlisted = append(unlisted, name)
			}
		}
	}
	sort.Strings(unlisted)
	for _, name := range unlisted {
		t.Errorf("%s loads a table through loadViewable, which only asks whether its actor may open it: a row write calls loadEditable (and joins tableWrites); a read joins tableReads with why", name)
	}

	found := 0
	for _, name := range tableWrites {
		c, ok := calls[name]
		if !ok {
			t.Errorf("row write %s isn't in the package any more: take it off tableWrites, or name what replaced it", name)
			continue
		}
		if !(c["loadEditable"] || c["loadEditables"]) || c["loadViewable"] {
			t.Errorf("row write %s doesn't load its table through loadEditable (or loadEditables) alone: a member who may only view the table could change it", name)
			continue
		}
		found++
	}
	if found < 12 {
		t.Errorf("found %d row writes on loadEditable, want at least 12", found)
	}
	for name := range tableReads {
		if _, ok := calls[name]; !ok {
			t.Errorf("read %s isn't in the package any more: take it off tableReads", name)
		}
	}
}
