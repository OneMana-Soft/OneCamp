package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// A handler that stores a file answers its failure through WriteStorageFailure
// or WriteUploadFailure, so a write that failed because storage is down says
// so instead of blaming the request.
//
// WHY. UploadFile answered every failure 400 "Failed to upload the file", in a
// shape the web doesn't read, and people with MinIO down were told to check
// their request. A handler written later would make the same choice unless
// something stops it. A handler stores a file when it calls PutObject itself,
// or one of the business functions that store an uploaded file.
func TestHandlersThatStoreFilesSayWhenStorageIsDown(t *testing.T) {
	stores := func(name string) bool {
		return name == "PutObject" || name == "SafeUploadToMinio" || strings.HasPrefix(name, "UploadUserFile")
	}
	answers := map[string]bool{"WriteStorageFailure": true, "WriteUploadFailure": true}

	found := 0
	err := filepath.WalkDir("../controllers", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var storing, answering bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				switch f := call.Fun.(type) {
				case *ast.SelectorExpr:
					name = f.Sel.Name
				case *ast.Ident:
					name = f.Name
				}
				storing = storing || stores(name)
				answering = answering || answers[name]
				return true
			})
			if !storing {
				continue
			}
			found++
			if !answering {
				t.Errorf("%s: %s stores a file but answers its failure without WriteStorageFailure or WriteUploadFailure", path, fn.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// UploadFile, UploadEmailLogo and the Slack import's HandleUpload at least:
	// fewer means the walk stopped seeing them, not that they went away.
	if found < 3 {
		t.Fatalf("found %d handlers that store a file; the guard is no longer looking at them", found)
	}
}
