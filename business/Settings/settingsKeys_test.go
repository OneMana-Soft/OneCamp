package business

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Every key this package declares is one loadAll reads. The audit retention
// window and the Firebase credential were saved by Admin and never read back,
// because the list of keys loadAll asked for didn't name them.
func TestEveryKeyIsLoaded(t *testing.T) {
	files, err := parser.ParseDir(token.NewFileSet(), ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := 0
	for _, pkg := range files {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				g, ok := d.(*ast.GenDecl)
				if !ok || g.Tok != token.CONST {
					continue
				}
				for _, spec := range g.Specs {
					v := spec.(*ast.ValueSpec)
					for i, name := range v.Names {
						if !strings.HasPrefix(name.Name, "key") || i >= len(v.Values) {
							continue
						}
						lit, ok := v.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						declared++
						key, _ := strconv.Unquote(lit.Value)
						if !slices.Contains(settingsKeys, key) {
							t.Errorf("%s (%q) is saved but loadAll never reads it: add it to settingsKeys", name.Name, key)
						}
					}
				}
			}
		}
	}
	if declared < len(settingsKeys) {
		t.Fatalf("found %d key constants for %d settingsKeys: the test no longer sees them all", declared, len(settingsKeys))
	}
}
