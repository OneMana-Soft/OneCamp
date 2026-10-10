package notification

// Every notification names whoever acted by the one name rule
// (DgraphUser.DisplayName, or helpers.PersonDisplayName): push titles and
// sender names, email senders and subjects (the actor name every Dispatch*
// takes), and webhook author names. A bare display name is empty for someone
// without one, and a full name first was a second rule. This reads the
// business and controller sources and refuses a user's .UserName or
// .UserFullName in what is handed to a Dispatch* or send*Notification call,
// put in a push field, or built into a title.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	notifyCall = regexp.MustCompile(`^(Dispatch[A-Z]\w*|send\w*Notification)$`)
	titleVar   = regexp.MustCompile(`(?i)title`)
)

func TestNotificationsNameActorsByTheNameRule(t *testing.T) {
	for _, root := range []string{"../../business", "../../controllers"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			report := func(n ast.Node, what string) {
				for _, sel := range rawNames(n) {
					t.Errorf("%s: %s reads .%s; use DisplayName()", fset.Position(sel.Pos()), what, sel.Sel.Name)
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if name := calleeName(x.Fun); notifyCall.MatchString(name) {
						for _, a := range x.Args {
							report(a, name)
						}
					}
				case *ast.AssignStmt:
					for i, lhs := range x.Lhs {
						if i >= len(x.Rhs) {
							break
						}
						if isPushField(lhs) || isTitleVar(lhs) {
							report(x.Rhs[i], "a push field or title")
						}
					}
				case *ast.KeyValueExpr:
					if isPushField(x.Key) {
						report(x.Value, "a push field")
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// isPushField reports whether e is a push data field that names someone:
// pushData[...USERNAME] or [...TITLE], or such a key.
func isPushField(e ast.Expr) bool {
	if ix, ok := e.(*ast.IndexExpr); ok {
		e = ix.Index
	}
	sel, ok := e.(*ast.SelectorExpr)
	return ok && (sel.Sel.Name == "FIREBASE_PUSH_DATA_USERNAME" || sel.Sel.Name == "FIREBASE_PUSH_DATA_TITLE")
}

func isTitleVar(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && titleVar.MatchString(id.Name)
}

// rawNames is every .UserName or .UserFullName read in n, except inside a
// call to PersonDisplayName, which is the rule.
func rawNames(n ast.Node) []*ast.SelectorExpr {
	var out []*ast.SelectorExpr
	ast.Inspect(n, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && calleeName(c.Fun) == "PersonDisplayName" {
			return false
		}
		if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "UserName" || sel.Sel.Name == "UserFullName") {
			out = append(out, sel)
		}
		return true
	})
	return out
}

func TestAnActorWithNoNameIsSomeone(t *testing.T) {
	if got := actorOrSomeone("  "); got != "Someone" {
		t.Fatalf("got %q", got)
	}
	if got := actorOrSomeone(" Sam "); got != "Sam" {
		t.Fatalf("got %q", got)
	}
}
