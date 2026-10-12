package domain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Every doc query whose answer reaches an access rule (CanRead, CanEdit,
// CanComment in business/Doc) reads the doc's deletion time: the rules refuse
// a deleted doc, and one read without the time reads as live. Three of these
// read nothing about deletion, and a doc in the trash could be opened, edited,
// joined live and read by the assistant (Oct 2026).
func TestDocAccessQueriesReadTheDeletionTime(t *testing.T) {
	sources := map[string][]string{
		"docDomain.go": {
			"GetDgraphDocByUUIDOnlyEditingInfo", // edits, the live editor, sharing links
			"GetBasicDgraphDocByUUID",           // comments, files, history, views, live updates
			"GetDgraphDocByUUID",                // opening a doc, the assistant's read_doc
			"GetDgraphDocAllCommentList",        // a doc's comments
			"GetDocPermissions",                 // who it's shared with, MCP's reach
			"GetDocDeletion",                    // a guest's link
			"GetCollabDocByUUID",                // the collaboration service opening it
			"GetSystemDocByUUID",                // a save that keeps the body (collab)
		},
		"../Comment/commentDomain.go": {
			"GetDgraphDocCommentInfoByUUID", // a comment's doc: edits, removals, reactions
		},
	}
	for file, funcs := range sources {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		bodies := map[string]string{}
		for _, d := range parsed.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
				bodies[fn.Name.Name] = string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
			}
		}
		for _, name := range funcs {
			body, ok := bodies[name]
			if !ok {
				t.Errorf("%s: no func %s (renamed? update this list)", file, name)
				continue
			}
			if !strings.Contains(body, "doc_deleted_at") {
				t.Errorf("%s reads a doc for an access rule without its deletion time: a deleted doc reads as live", name)
			}
		}
	}
}
