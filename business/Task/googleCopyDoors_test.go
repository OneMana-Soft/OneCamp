package business

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// Every door in the module that writes a task's Google copy makes one call
// (callers are found by the Integration package's import path, under any
// alias; a door outside business/Task is named by its directory): a door that hands the task to someone else goes through
// MoveTaskGoogleCopy (out of the old assignee's calendar, then into the new
// assignee's, in order, V-1114); the archive door goes through
// ArchiveTaskGoogleCopy (out of the assignee's calendar whatever their task
// sync says, V-1119); a door that keeps the assignee re-syncs that one person.
// The hand-over and the archive take the task's place in its line at the call,
// never from a goroutine started for it, and hand their pass
// pgexec.Detached(ctx): the request's values (trace, log), never its cancel or
// a transaction it holds (V-1119 F1). A new door joins the table.
func TestEveryGoogleCopyDoorMakesItsOneCall(t *testing.T) {
	const move, sync, archive = "MoveTaskGoogleCopy", "SyncTaskToGoogleCalendar", "ArchiveTaskGoogleCopy"
	doors := map[string]string{
		"UpdateTaskAssigneeByTaskUUID": move, // the hand-over
		"CreateSubTask":                sync, // the rest keep the assignee
		"CreateTaskAsked":              sync,
		"UpdateTaskDesByTaskUUID":      sync,
		"UpdateTaskNameByTaskUUID":     sync,
		"afterDates":                   sync,
		"ArchiveTaskByTaskUUID":        archive,
		"UnArchiveTaskByTaskUUID":      sync,
		"afterMove":                    sync,
	}
	const integrationPath = "github.com/akashc777/OneCamp/business/Integration"
	const pgexecPath = "github.com/akashc777/OneCamp/models/pgexec"
	root := filepath.Join("..", "..")
	found, inGo, passCtx := map[string][]string{}, map[string]bool{}, map[string]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		head, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		alias, pg := "", ""
		for _, imp := range head.Imports {
			if strings.Trim(imp.Path.Value, `"`) == pgexecPath {
				pg = "pgexec"
				if imp.Name != nil {
					pg = imp.Name.Name
				}
			}
			if strings.Trim(imp.Path.Value, `"`) == integrationPath {
				alias = "Integration"
				if imp.Name != nil {
					alias = imp.Name.Name
				}
			}
		}
		if alias == "" {
			return nil
		}
		dir, _ := filepath.Rel(root, filepath.Dir(path))
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			door := fn.Name.Name
			if dir != filepath.Join("business", "Task") {
				door = dir + "." + door
			}
			var goStmts []*ast.GoStmt
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if g, ok := n.(*ast.GoStmt); ok {
					goStmts = append(goStmts, g)
				}
				if call, ok := n.(*ast.CallExpr); ok && len(call.Args) > 0 {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == move || sel.Sel.Name == archive) {
						if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == alias && !detached(call.Args[0], pg) {
							passCtx[door] = types.ExprString(call.Args[0])
						}
					}
				}
				if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == move || sel.Sel.Name == sync || sel.Sel.Name == archive) {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == alias {
						found[door] = append(found[door], sel.Sel.Name)
						for _, g := range goStmts {
							if g.Pos() <= sel.Pos() && sel.End() <= g.End() && sel.Sel.Name != sync {
								inGo[door] = true
							}
						}
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for door, want := range doors {
		if calls := found[door]; len(calls) != 1 || calls[0] != want {
			t.Errorf("%s calls %v, want one %s", door, calls, want)
		}
		if got, ok := passCtx[door]; ok {
			t.Errorf("%s hands %s %s, want pgexec.Detached(ctx): the request's values without its cancel or its transaction", door, want, got)
		}
		if inGo[door] {
			t.Errorf("%s calls %s from a goroutine, want its place in the task's line taken at the call", door, want)
		}
	}
	for fn, calls := range found {
		if _, ok := doors[fn]; !ok {
			t.Errorf("%s writes a task's Google copy (%v) and isn't a door in this table", fn, calls)
		}
	}
}

// detached reports whether arg is pgexec.Detached(...), pg being the name the
// file imports pgexec under ("" when it doesn't).
func detached(arg ast.Expr, pg string) bool {
	call, ok := arg.(*ast.CallExpr)
	if !ok || pg == "" {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Detached" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == pg
}

// A bulk delete archives each task through the archive door with the task it
// read, so the door finds its assignee and takes the Google copy out (V-1119
// F3): bulk_test swaps bulkSeams whole, so changeOne never runs in units.
func TestBulkDeleteHandsTheArchiveDoorTheTaskItRead(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "bulk.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var changeOne *ast.FuncDecl
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "changeOne" && fn.Body != nil {
			changeOne = fn
		}
	}
	if changeOne == nil {
		t.Fatal("bulk.go has no changeOne")
	}
	read := ""
	for _, field := range changeOne.Type.Params.List {
		if types.ExprString(field.Type) == "*dgraphStruct.DgraphTask" && len(field.Names) == 1 {
			read = field.Names[0].Name
		}
	}
	if read == "" {
		t.Fatal("changeOne takes no *dgraphStruct.DgraphTask")
	}
	var handed []string
	ast.Inspect(changeOne.Body, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok {
			for _, lhs := range as.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == read {
					t.Errorf("changeOne gives %s a new value (%s), want the task it read", read, types.ExprString(as.Rhs[0]))
				}
			}
		}
		cc, ok := n.(*ast.CaseClause)
		if !ok || len(cc.List) != 1 || types.ExprString(cc.List[0]) != "BulkDelete" {
			return true
		}
		ast.Inspect(&ast.BlockStmt{List: cc.Body}, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && types.ExprString(call.Fun) == "ArchiveTaskByTaskUUID" && len(call.Args) > 2 {
				handed = append(handed, types.ExprString(call.Args[2]))
			}
			return true
		})
		return true
	})
	if len(handed) != 1 || handed[0] != read {
		t.Errorf("changeOne's BulkDelete hands ArchiveTaskByTaskUUID %v, want the one task it read (%s)", handed, read)
	}
}
