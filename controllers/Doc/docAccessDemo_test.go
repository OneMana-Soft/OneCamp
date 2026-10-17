package docController

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The demo's shared visitor owns the demo's docs (business/DemoSeed makes them
// as that account), and every visitor after them opens the same docs. Every
// handler that changes who may open a doc, who owns it, or its lock asks the
// demo guard first, so one visitor can't lock the Q4 plan or hand it away for
// everyone until the nightly reset.
func TestEveryAccessChangeAsksTheDemoGuard(t *testing.T) {
	src, err := os.ReadFile("docAccessController.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "docAccessController.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{}
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
			bodies[fn.Name.Name] = string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
		}
	}
	for _, name := range []string{"UpdateDocAccess", "UpdateDocSharing", "TransferDoc", "LockDoc"} {
		body, ok := bodies[name]
		if !ok {
			t.Errorf("no handler %s (renamed? update this list)", name)
			continue
		}
		guard := strings.Index(body, "refuseDemo(")
		if guard < 0 {
			t.Errorf("%s changes a doc's access without asking the demo guard", name)
			continue
		}
		for _, change := range []string{"accessBusiness.Apply(", "accessBusiness.Lock(", "accessBusiness.Rules.Transfer("} {
			if i := strings.Index(body, change); i >= 0 && i < guard {
				t.Errorf("%s makes its change before asking the demo guard", name)
			}
		}
	}
	if !strings.Contains(bodies["RemoveDocComment"], "demoGuard.KeepsFromVisitor(") {
		t.Error("a moderator on the demo can take the demo's own comments down")
	}
}
