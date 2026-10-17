package docController

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// router.go still declares POST /doc/updateDocPermissions for the old
// handler, and A5's mount answers it with UpdateDocSharing (chi keeps the
// last registration). Whichever answers, a change to sharing goes by the
// Access Resolver's rules: the old handler is UpdateDocSharing and nothing
// else, and no handler changes sharing through the old business path, which
// asks only "is this the owner" (the demo's seed alone, server side, uses it).
func TestTheOldSharingRouteGoesByA5sRules(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "docController.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "UpdateDocPermissions" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("no UpdateDocPermissions handler (renamed? update this test)")
	}
	ok := len(body.List) == 1
	if ok {
		call, isCall := body.List[0].(*ast.ExprStmt)
		ok = isCall
		if ok {
			fn, isFn := call.X.(*ast.CallExpr)
			ident, isIdent := fn.Fun.(*ast.Ident)
			ok = isFn && isIdent && ident.Name == "UpdateDocSharing" && len(fn.Args) == 2
		}
	}
	if !ok {
		t.Error("UpdateDocPermissions does more than hand the request to UpdateDocSharing")
	}

	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "business.UpdateDocPermissions(") {
			t.Errorf("%s changes sharing through the old business path", f)
		}
	}
}
