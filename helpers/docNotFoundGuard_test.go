package helpers

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A doc that isn't there for someone is answered in one sentence, from one
// place (DocNotFound): the doc doesn't exist, or they can't open it. A 404
// never tells the two apart, so no answer may word either on its own.
//
// WHAT FAILS. A string inside a function, anywhere in the module's Go code,
// that words a missing doc any other way ("There's no such doc.", "Doc not
// found", "document not found or you don't have access"); and a call of
// WriteReadFailure for a doc anywhere but WriteDocReadFailure, since its
// own 404 would say "Doc not found".
//
// WHAT DOESN'T. Package-level declarations: a named error's text is the
// error's own (ErrDocAINotFound, a model's ErrNotFound), matched by
// errors.Is and never answered as it stands, and a named constant may hold
// another service's text that is matched, not said (services/collab's
// serviceNoSuchDoc). Comments are not strings.
func TestADocThatIsntThereIsAnsweredOneWay(t *testing.T) {
	roots := []string{
		"../adapter", "../business", "../cmd", "../controllers", "../domain", "../helpers",
		"../initializers", "../middleware", "../models", "../router", "../services",
	}
	missingDoc := regexp.MustCompile(`(?i)(no such doc\b|\bdoc(ument)? not found|\bdoc(ument)? (doesn't|does not|didn't|did not) exist|can't open (the|this|that) doc\b)`)
	var bad []string
	files, readFailureForADoc := 0, 0
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("%s isn't there to scan: %v", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "node_modules" || name == "vendor" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			files++
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue // package-level: a named error or constant
				}
				ast.Inspect(fn, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.BasicLit:
						if n.Kind != token.STRING {
							return true
						}
						s, err := strconv.Unquote(n.Value)
						if err == nil && missingDoc.MatchString(s) {
							bad = append(bad, fmt.Sprintf("%s: %q words a missing doc on its own; answer with helpers.DocNotFound", fset.Position(n.Pos()), s))
						}
					case *ast.CallExpr:
						if !callsWriteReadFailure(n) || len(n.Args) < 4 {
							return true
						}
						if what, ok := n.Args[3].(*ast.BasicLit); ok && what.Value == `"doc"` {
							if fn.Name.Name == "WriteDocReadFailure" && f.Name.Name == "helpers" {
								readFailureForADoc++
								return true
							}
							bad = append(bad, fmt.Sprintf("%s: WriteReadFailure for a doc would answer \"Doc not found\"; use helpers.WriteDocReadFailure", fset.Position(n.Pos())))
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
	}
	if files < 500 || readFailureForADoc != 1 {
		t.Fatalf("scanned %d files and found WriteDocReadFailure's own call %d times: the scan isn't reaching the code", files, readFailureForADoc)
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
}

// callsWriteReadFailure says whether a call is WriteReadFailure's, from this
// package or another (helpers.WriteReadFailure).
func callsWriteReadFailure(c *ast.CallExpr) bool {
	switch fun := c.Fun.(type) {
	case *ast.Ident:
		return fun.Name == "WriteReadFailure"
	case *ast.SelectorExpr:
		return fun.Sel.Name == "WriteReadFailure"
	}
	return false
}
