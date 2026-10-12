package controllers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A reaction is changed or removed by the uid a request names
// (reaction_dgraph_id), and until October 2026 that uid was trusted: naming
// any node, someone's private doc say, stamped it as the caller's reaction,
// and removing that "reaction" wiped the node. reactionBusiness.MayChange
// checks the uid against the parent the request names and the caller; this
// keeps every handler that changes or removes a reaction calling it first.
func TestReactionHandlersCheckTheUidFirst(t *testing.T) {
	changes := map[string]bool{
		"CreateOrUpdateDocCommentReaction": true, "CreateOrUpdatePostCommentReaction": true,
		"CreateOrUpdatePostReaction": true, "CreateOrUpdateChatReaction": true,
		"CreateOrUpdateChatCommentReaction": true, "CreateOrUpdateTaskCommentReaction": true,
		"DeleteReactionOnCommentDoc": true, "DeleteReactionOnCommentPost": true, "DeletePostReaction": true,
		"DeleteChatReaction": true, "DeleteReactionOnCommentChat": true, "DeleteReactionOnCommentTask": true,
	}
	found := 0
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			checked := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "reactionBusiness" && sel.Sel.Name == "MayChange" {
					checked = true
				} else if changes[sel.Sel.Name] {
					found++
					if !checked {
						t.Errorf("%s: %s calls %s without reactionBusiness.MayChange before it",
							fset.Position(call.Pos()), fn.Name.Name, sel.Sel.Name)
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found < len(changes) {
		t.Fatalf("found only %d calls that change or remove a reaction, want at least %d; the scan is looking in the wrong place", found, len(changes))
	}
}
