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

// A handler that writes an error response stops there.
//
// WHY THIS IS ENFORCED. Editing or deleting someone else's thread reply wrote
// "403 Not Authorized" and then carried on, rewriting or deleting the reply
// anyway: the person saw the refusal while the change was already made. Six
// more handlers had the same shape (a refused channel name went on to index
// an empty list; a call to someone who doesn't exist went on to start it).
// Nothing about such code looks wrong in a diff, so it is checked here.
//
// The rule: after helpers.WriteJSON, w.WriteHeader or http.Error with a 4xx or
// 5xx status, the code must leave (return, break, continue, panic) before it
// does anything else, unless nothing else is left to run in the function.

// guardErrorStatus reports whether e is http.Status... for a 4xx or 5xx code.
func guardErrorStatus(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "http" {
		return false
	}
	code, known := httpStatusCodes[sel.Sel.Name]
	return known && code >= 400
}

// httpStatusCodes is every net/http status name a handler here might use.
var httpStatusCodes = map[string]int{
	"StatusContinue": 100, "StatusSwitchingProtocols": 101,
	"StatusOK": 200, "StatusCreated": 201, "StatusAccepted": 202, "StatusNoContent": 204, "StatusPartialContent": 206,
	"StatusMovedPermanently": 301, "StatusFound": 302, "StatusSeeOther": 303, "StatusNotModified": 304,
	"StatusTemporaryRedirect": 307, "StatusPermanentRedirect": 308,
	"StatusBadRequest": 400, "StatusUnauthorized": 401, "StatusPaymentRequired": 402, "StatusForbidden": 403,
	"StatusNotFound": 404, "StatusMethodNotAllowed": 405, "StatusNotAcceptable": 406, "StatusRequestTimeout": 408,
	"StatusConflict": 409, "StatusGone": 410, "StatusLengthRequired": 411, "StatusPreconditionFailed": 412,
	"StatusRequestEntityTooLarge": 413, "StatusUnsupportedMediaType": 415, "StatusTeapot": 418,
	"StatusUnprocessableEntity": 422, "StatusLocked": 423, "StatusFailedDependency": 424, "StatusTooEarly": 425,
	"StatusPreconditionRequired": 428, "StatusTooManyRequests": 429, "StatusRequestHeaderFieldsTooLarge": 431,
	"StatusUnavailableForLegalReasons": 451,
	"StatusInternalServerError":        500, "StatusNotImplemented": 501, "StatusBadGateway": 502,
	"StatusServiceUnavailable": 503, "StatusGatewayTimeout": 504, "StatusInsufficientStorage": 507,
}

// guardWritesError reports whether a statement writes an error response.
func guardWritesError(st ast.Stmt) bool {
	es, ok := st.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "WriteJSON":
		return len(call.Args) >= 2 && guardErrorStatus(call.Args[1])
	case "WriteHeader":
		return len(call.Args) == 1 && guardErrorStatus(call.Args[0])
	case "Error":
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "http" && len(call.Args) == 3 {
			return guardErrorStatus(call.Args[2])
		}
	}
	return false
}

func guardLeaves(st ast.Stmt) bool {
	switch s := st.(type) {
	case *ast.ReturnStmt, *ast.BranchStmt:
		return true
	case *ast.ExprStmt:
		if c, ok := s.X.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "panic" {
				return true
			}
		}
	}
	return false
}

// errorWritesThatCarryOn returns the lines of a file's error responses that
// the code doesn't leave after.
func errorWritesThatCarryOn(fset *token.FileSet, f *ast.File) []int {
	var lines []int
	// walk checks one statement list; ends says nothing runs after it but
	// the function's end or a statement that leaves.
	var walk func(list []ast.Stmt, ends bool)
	walk = func(list []ast.Stmt, ends bool) {
		for i, st := range list {
			if guardWritesError(st) {
				// Fine when it is the last thing in a list after which nothing
				// runs, or when a statement that leaves follows it.
				left := ends && i == len(list)-1
				for _, rest := range list[i+1:] {
					if left || guardLeaves(rest) {
						left = true
						break
					}
				}
				if !left {
					lines = append(lines, fset.Position(st.Pos()).Line)
				}
			}
			after := (ends && i == len(list)-1) || (i+1 < len(list) && guardLeaves(list[i+1]))
			switch x := st.(type) {
			case *ast.IfStmt:
				for x != nil {
					walk(x.Body.List, after)
					next, _ := x.Else.(*ast.IfStmt)
					if blk, ok := x.Else.(*ast.BlockStmt); ok {
						walk(blk.List, after)
					}
					x = next
				}
			case *ast.SwitchStmt:
				for _, c := range x.Body.List {
					walk(c.(*ast.CaseClause).Body, after)
				}
			case *ast.TypeSwitchStmt:
				for _, c := range x.Body.List {
					walk(c.(*ast.CaseClause).Body, after)
				}
			case *ast.SelectStmt:
				for _, c := range x.Body.List {
					walk(c.(*ast.CommClause).Body, after)
				}
			case *ast.BlockStmt:
				walk(x.List, after)
			case *ast.ForStmt:
				walk(x.Body.List, false)
			case *ast.RangeStmt:
				walk(x.Body.List, false)
			case *ast.LabeledStmt:
				walk([]ast.Stmt{x.Stmt}, after)
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch fn := n.(type) {
		case *ast.FuncDecl:
			if fn.Body != nil {
				walk(fn.Body.List, true)
			}
		case *ast.FuncLit:
			walk(fn.Body.List, true)
		}
		return true
	})
	return lines
}

func TestAnErrorResponseIsTheLastThingAHandlerDoes(t *testing.T) {
	checked := 0
	for _, root := range []string{"../controllers", "../middleware", "../router"} {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("scan root %s missing, so this guard would pass having checked nothing: %v", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			checked++
			for _, line := range errorWritesThatCarryOn(fset, f) {
				t.Errorf("%s:%d writes an error response and carries on: add a return after it", path, line)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 50 {
		t.Fatalf("only %d files checked; the handlers have moved and this guard no longer sees them", checked)
	}
}

// The guard itself: it finds the shapes that were bugs, and lets through the
// shapes that aren't.
func TestTheErrorResponseGuardSeesWhatItShould(t *testing.T) {
	src := `package x
import "net/http"
func refusedButCarriesOn(w http.ResponseWriter) {
	if notAuthor {
		helpers.WriteJSON(w, http.StatusForbidden, nil) // BAD
	}
	update()
}
func nextStatementGoesOn(w http.ResponseWriter) {
	helpers.WriteJSON(w, http.StatusBadRequest, nil) // BAD
	update()
}
func returns(w http.ResponseWriter) {
	if notAuthor {
		helpers.WriteJSON(w, http.StatusForbidden, nil)
		return
	}
	update()
}
func logsThenReturns(w http.ResponseWriter) {
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log(err)
		return
	}
}
func endsTheFunction(w http.ResponseWriter, err error) {
	switch {
	case a:
		helpers.WriteJSON(w, http.StatusConflict, nil)
	default:
		http.Error(w, "x", http.StatusInternalServerError)
	}
}
func switchThenReturn(w http.ResponseWriter) {
	if err != nil {
		switch {
		case a:
			helpers.WriteJSON(w, http.StatusBadRequest, nil)
		}
		return
	}
	update()
}
func okStatusIsFine(w http.ResponseWriter) {
	helpers.WriteJSON(w, http.StatusOK, nil)
	update()
}
func inALoop(w http.ResponseWriter) {
	for _, x := range xs {
		helpers.WriteJSON(w, http.StatusBadRequest, nil) // BAD
	}
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var want []int
	for i, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "// BAD") {
			want = append(want, i+1)
		}
	}
	got := errorWritesThatCarryOn(fset, f)
	if len(got) != len(want) {
		t.Fatalf("flagged lines %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("flagged lines %v, want %v", got, want)
		}
	}
}
