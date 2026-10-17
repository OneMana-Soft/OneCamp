package docController

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The share dialog's General access, sent to /doc/updateDoc, is one change to
// the doc's sharing (A5's Apply): the general access its stored settings make
// with the dialog's in place, and nothing when that is what it has already.
func TestTheGeneralAccessMenuIsOneChangeToSharing(t *testing.T) {
	yes, no := true, false
	open := &dgraphStruct.DgraphDoc{Uuid: "d0c00000-0000-4000-8000-000000000001", IsPrivate: &no, PublicComment: &no}
	for _, c := range []struct {
		name    string
		doc     *dgraphStruct.DgraphDoc
		in      adapter.InputUpdateDoc
		private bool
		level   accessBusiness.Level
	}{
		{"Restricted", open, adapter.InputUpdateDoc{IsPrivate: &yes, PublicComment: &no}, true, accessBusiness.LevelView},
		{"everyone may comment", open, adapter.InputUpdateDoc{IsPrivate: &no, PublicComment: &yes}, false, accessBusiness.LevelComment},
		{"privacy alone keeps a space's edit", &dgraphStruct.DgraphDoc{Uuid: open.Uuid, IsPrivate: &no, PublicEdit: &yes}, adapter.InputUpdateDoc{IsPrivate: &yes}, true, accessBusiness.LevelEdit},
		{"open again", &dgraphStruct.DgraphDoc{Uuid: open.Uuid, IsPrivate: &yes}, adapter.InputUpdateDoc{IsPrivate: &no, PublicComment: &no}, false, accessBusiness.LevelView},
	} {
		got := generalChange(c.doc, c.in)
		if got == nil || got.Private == nil || got.General == nil {
			t.Errorf("%s: %+v", c.name, got)
			continue
		}
		if *got.Private != c.private || *got.General != c.level || got.Resource != docRes(open.Uuid) || len(got.Add) != 0 || len(got.Remove) != 0 {
			t.Errorf("%s: private %v at %s on %+v, want private %v at %s", c.name, *got.Private, *got.General, got.Resource, c.private, c.level)
		}
	}
	if got := generalChange(open, adapter.InputUpdateDoc{IsPrivate: &no, PublicComment: &no}); got != nil {
		t.Errorf("the dialog sending what the doc has already changes %+v", got)
	}
}

// A change to who can open a doc through /doc/updateDoc, or a move that
// changes a page's space, asks the demo guard before it is made, as every
// change to sharing does (docAccessDemo_test.go).
func TestSharingThroughTheEditorAndTheTreeAsksTheDemoGuard(t *testing.T) {
	for file, handlers := range map[string]map[string]string{
		"docBodyController.go": {"UpdateDocBodyLive": "accessBusiness.Apply("},
		"docTreeController.go": {"MoveDoc": "treeBusiness.Hierarchy.Move("},
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range parsed.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			change, ok := handlers[fn.Name.Name]
			if !ok {
				continue
			}
			delete(handlers, fn.Name.Name)
			body := string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
			guard, at := strings.Index(body, "refuseDemo("), strings.Index(body, change)
			if guard < 0 || at < 0 || at < guard {
				t.Errorf("%s makes its change (%s) without asking the demo guard first", fn.Name.Name, change)
			}
		}
		for name := range handlers {
			t.Errorf("no handler %s in %s (renamed? update this list)", name, file)
		}
	}
}
