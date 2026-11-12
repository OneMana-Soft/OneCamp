package business

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Presenter notes are the presenter's (miro-28 2a): no export, AI context,
// search entry, MCP or API tool, audit text or guest route reads them. Each of
// those reads a board through its own code, so the notes store is imported by
// one file of the server, business/Board/notes.go, which reads them only for
// who may present the board. A new reader fails here, and is the moment to
// ask whether it may.
func TestOnlyTheBoardsNotesReadTheNotesStore(t *testing.T) {
	const store = "github.com/akashc777/OneCamp/models/postgres/BoardNotes"
	root := filepath.Join("..", "..")
	var readers []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "public-repo", "vendor", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == store {
				rel, _ := filepath.Rel(root, path)
				readers = append(readers, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(readers)
	if got := strings.Join(readers, ","); got != "business/Board/notes.go" {
		t.Fatalf("the notes store is read by [%s], want business/Board/notes.go alone", got)
	}
}
