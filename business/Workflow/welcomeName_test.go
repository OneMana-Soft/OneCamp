package business

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// A welcome names whoever joined by the one name rule without its last step,
// never by DisplayName(), whose last step is part of their address: it posts
// into the channel they joined, which may have guests.
func TestAWelcomeNeverNamesTheJoinerByTheirAddress(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "workflowBusiness.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var welcome *ast.FuncDecl
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "handleUserJoinedChannel" {
			welcome = fn
		}
	}
	if welcome == nil {
		t.Fatal("handleUserJoinedChannel not found")
	}
	named := false
	ast.Inspect(welcome.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "DisplayName":
			t.Errorf("%s: the joiner is named with DisplayName()", fset.Position(call.Pos()))
		case "PersonDisplayName":
			named = true
			if last, ok := call.Args[len(call.Args)-1].(*ast.BasicLit); !ok || last.Value != `""` {
				t.Errorf(`%s: the joiner is named with an address; pass ""`, fset.Position(call.Pos()))
			}
		}
		return true
	})
	if !named {
		t.Error("the welcome no longer names the joiner by helpers.PersonDisplayName")
	}
}
