package controllers

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"
)

// V-126: both DM call routes ask who else is in the call before any room or
// token, and answer a call with only agents as a refusal (business/Chat,
// through helpers.WriteCallRefusal), so no client can start one, whatever it
// offers. POST /dm/getCallToken knows its other by uuid (RefuseCallWith);
// POST /groupChat/getCallToken takes any DM's id, a pair's included, and asks
// by the room's members (RefuseCallIn; CR-178's F1).
func TestADMCallAsksWhoItIsWithBeforeAnyRoom(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "chatController.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	for route, rule := range map[string]string{"MakeVideoCallForChat": "RefuseCallWith", "MakeVideoCallForGroup": "RefuseCallIn"} {
		var fn *ast.FuncDecl
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == route {
				fn = fd
			}
		}
		if fn == nil {
			t.Fatalf("%s not found", route)
		}
		isCall := func(n ast.Expr, pkg, name string) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return false
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			x, ok := sel.X.(*ast.Ident)
			return ok && x.Name == pkg && sel.Sel.Name == name
		}
		var refuse, room token.Pos
		held := map[string]bool{} // names the rule's answer is assigned to
		var answers []ast.Expr    // what helpers.WriteCallRefusal is handed
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				if len(n.Rhs) == 1 && isCall(n.Rhs[0], "business", rule) {
					for _, l := range n.Lhs {
						if id, ok := l.(*ast.Ident); ok {
							held[id.Name] = true
						}
					}
				}
			case *ast.CallExpr:
				switch {
				case isCall(n, "business", rule) && refuse == 0:
					refuse = n.Pos()
				case isCall(n, "business", "MakeVideoCall") && room == 0:
					room = n.Pos()
				case isCall(n, "helpers", "WriteCallRefusal") && len(n.Args) == 2:
					answers = append(answers, n.Args[1])
				}
			}
			return true
		})
		answered := false
		for _, a := range answers {
			var b strings.Builder
			_ = printer.Fprint(&b, fset, a)
			answered = answered || strings.HasPrefix(b.String(), "business."+rule+"(") || held[b.String()]
		}
		if refuse == 0 || !answered {
			t.Errorf("%s never answers business.%s through helpers.WriteCallRefusal", route, rule)
			continue
		}
		if room == 0 || refuse > room {
			t.Errorf("%s asks who else is in the call after making its room (refuse %v, room %v)", route, fset.Position(refuse), fset.Position(room))
		}
	}
}
