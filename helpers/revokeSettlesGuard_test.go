package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// Search grants a doc or a board through the sharing its index entry holds,
// and counts by it (docs C2-1, D-23). So every change that can take one from
// someone writes the entry, and waits until search sees it, before it
// returns: otherwise a count goes on including what the person just lost
// until a background write catches up. Each path below calls its settled
// writer in its own flow, never from a goroutine. A new path that takes
// access away belongs on this list. Channels, projects and teams need none:
// search reads them from the asker's own sign-in.
var revokeSettles = []struct {
	file, fn, settles string
}{
	// Every change to docs' sharing: share and unshare, the workspace's level,
	// a space's people, a move that takes the new parent's, a grant that ran
	// out, an access request's answer.
	{"business/Doc/docAccessApply.go", "AccessChanged", "SettleAccessIndexes"},
	// Docs into the trash: the settled cascade, under its own bound.
	{"business/Doc/docTrash.go", "searchFollowsTrash", "trashSettled"},
	{"business/Doc/docTrash.go", "trashSettled", "cascadeDeleteNow"},
	// A board unshared, made private, deleted.
	{"business/Board/boardBusiness.go", "UpdateBoardPermissions", "SettleBoardSearch"},
	{"business/Board/boardBusiness.go", "UpdateBoard", "SettleBoardSearch"},
	{"business/Board/boardBusiness.go", "DeleteBoard", "settleBoardDelete"},
}

func TestEveryRevocationSettlesSearchBeforeItReturns(t *testing.T) {
	for _, r := range revokeSettles {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join("..", r.file), nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", r.file, err)
		}
		var body *ast.BlockStmt
		for _, d := range file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == r.fn && fd.Body != nil {
				body = fd.Body
			}
		}
		if body == nil {
			t.Errorf("%s has no func %s: a revocation path moved; pin where it went", r.file, r.fn)
			continue
		}
		inFlow, inBackground := false, false
		var walk func(n ast.Node, background bool)
		walk = func(n ast.Node, background bool) {
			ast.Inspect(n, func(c ast.Node) bool {
				switch c := c.(type) {
				case *ast.GoStmt:
					walk(c.Call, true)
					return false
				case *ast.FuncLit:
					walk(c.Body, true)
					return false
				case *ast.CallExpr:
					if funcName(c.Fun) == r.settles {
						if background {
							inBackground = true
						} else {
							inFlow = true
						}
					}
				}
				return true
			})
		}
		walk(body, false)
		switch {
		case inBackground:
			t.Errorf("%s %s calls %s from a goroutine or a func literal: it must settle before the change returns", r.file, r.fn, r.settles)
		case !inFlow:
			t.Errorf("%s %s doesn't call %s: search would go on granting what it took", r.file, r.fn, r.settles)
		}
	}
}
