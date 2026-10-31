package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// importDuplicates are the functions models/postgres/SlackImport still
// writes itself under a name models/postgres/Import has (backend audit U1b:
// about 25 when it was counted, 30 by name, 10 left. The Slack import is the generic
// one's legacy twin over the same tables; a function the two share should
// call the generic one. It only gets shorter: each of these differs in
// shape (its own Job or Chunk type, Slack's fixed provider) and is to be
// folded in, then deleted from this list.
var importDuplicates = []string{
	"ClaimNextChunk", "CreateChunks", "CreateJob", "GetJob", "IdMappingsByTypeOwned",
	"ListErrors", "LookupWorkspaceMapping", "SavePlan",
	"isUniqueViolation",
}

// funcBodies is the top-level functions of the non-test files in dir.
func funcBodies(t *testing.T, dir string) map[string]*ast.FuncDecl {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*ast.FuncDecl{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Body != nil {
				out[fd.Name.Name] = fd
			}
		}
	}
	return out
}

// callsOnly reports whether fd's body is one call (returned or not) of a
// function named name in another package.
func callsOnly(fd *ast.FuncDecl, name string) bool {
	if len(fd.Body.List) != 1 {
		return false
	}
	var call ast.Expr
	switch s := fd.Body.List[0].(type) {
	case *ast.ReturnStmt:
		if len(s.Results) != 1 {
			return false
		}
		call = s.Results[0]
	case *ast.ExprStmt:
		call = s.X
	default:
		return false
	}
	c, ok := call.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name
}

// The Slack import's models write no function the generic import's have
// that isn't a call of it, but for the shrinking list above.
func TestSlackImportModelsCallTheGenericOnes(t *testing.T) {
	generic := funcBodies(t, "../models/postgres/Import")
	slack := funcBodies(t, "../models/postgres/SlackImport")
	allowed := map[string]bool{}
	for _, n := range importDuplicates {
		allowed[n] = true
	}
	var dup, stale []string
	for name, fd := range slack {
		if generic[name] == nil || callsOnly(fd, name) {
			if allowed[name] {
				stale = append(stale, name)
			}
			continue
		}
		if !allowed[name] {
			dup = append(dup, name)
		}
	}
	for _, n := range importDuplicates {
		if slack[n] == nil {
			stale = append(stale, n)
		}
	}
	sort.Strings(dup)
	sort.Strings(stale)
	for _, n := range dup {
		t.Errorf("models/postgres/SlackImport.%s repeats models/postgres/Import.%s: call it", n, n)
	}
	for _, n := range stale {
		t.Errorf("%s is in importDuplicates but no longer repeats the generic one: delete it from the list", n)
	}
}
