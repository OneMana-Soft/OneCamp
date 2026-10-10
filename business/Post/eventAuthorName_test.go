package business

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// A post's events name its author by the one name rule without its last
// step, helpers.PersonDisplayName(display name, full name, ""), and never by
// DisplayName(), whose last step is part of their address: the Slack bridge
// (business/SlackBridge) posts under author_name in Slack, where a shared
// channel can hold people from other organisations.
func TestPostEventsNeverNameTheAuthorByTheirAddress(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "postBusiness.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	ast.Inspect(f, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if key, ok := kv.Key.(*ast.BasicLit); !ok || key.Value != `"author_name"` {
			return true
		}
		found++
		call, ok := kv.Value.(*ast.CallExpr)
		var callee *ast.SelectorExpr
		if ok {
			callee, _ = call.Fun.(*ast.SelectorExpr)
		}
		if callee == nil || callee.Sel.Name != "PersonDisplayName" || len(call.Args) != 3 || !isEmptyString(call.Args[2]) {
			t.Errorf(`%s: author_name must be helpers.PersonDisplayName(display name, full name, "")`, fset.Position(kv.Pos()))
		}
		return true
	})
	if found < 2 {
		t.Fatalf("found %d author_name entries, want post.created's and post.comment.created's", found)
	}
}

func isEmptyString(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
}
