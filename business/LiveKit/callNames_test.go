package business

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// A call shows each member by the one name rule (DgraphUser.DisplayName): the
// name on their tile in a call and the name of whoever started a recording.
// Both used to be the bare display name, which is empty for someone without
// one. Nothing in this file may read a user's UserName or UserFullName to
// name them.
func TestCallsNameMembersByTheNameRule(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "liveKitBusiness.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "UserName" || sel.Sel.Name == "UserFullName") {
			t.Errorf("liveKitBusiness.go reads .%s; name people with DisplayName()", sel.Sel.Name)
		}
		return true
	})
}
