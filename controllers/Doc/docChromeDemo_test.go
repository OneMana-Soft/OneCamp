package docController

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// An invite is a change to who may open a doc: it asks the demo guard before
// it is made, as A5's changes do, so one visitor can't share the demo's own
// docs for everyone after them.
func TestAnInviteAsksTheDemoGuardFirst(t *testing.T) {
	src, err := os.ReadFile("docChromeController.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "docChromeController.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body string
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "InviteToDoc" && fn.Body != nil {
			body = string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
		}
	}
	guard, change := strings.Index(body, "refuseDemo("), strings.Index(body, "accessBusiness.Invite(")
	switch {
	case body == "":
		t.Fatal("no handler InviteToDoc (renamed? update this test)")
	case guard < 0:
		t.Error("InviteToDoc shares a doc without asking the demo guard")
	case change < 0 || change < guard:
		t.Error("InviteToDoc makes its change before asking the demo guard")
	}
}
