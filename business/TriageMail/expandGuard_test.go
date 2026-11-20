package business

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// go-imap v2.0.0-beta.8 turns a number set into a slice by counting each range
// from Start to Stop in a uint32 (internal/imapnum Range.append). A range that
// ends at 4294967295 never ends: the counter wraps to 0 and the slice grows
// until the server runs out of memory. A set the other side sends is theirs to
// choose, so one ~20-byte "* SEARCH 4294967295" was enough (V-1233). The door
// reads a server's ranges itself (lowestAbove: at most searchRanges of them,
// never expanded). These are go-imap's expanding methods; no server code that
// imports go-imap may name them.
var imapExpanders = map[string]string{
	"Nums":       "SeqSet.Nums / UIDSet.Nums",
	"AllUIDs":    "SearchData.AllUIDs",
	"AllSeqNums": "SearchData.AllSeqNums",
}

const imapModule = "github.com/emersion/go-imap/v2"

// imapExpansions lists where a file that imports go-imap names an expanding
// method, called or taken as a value. A file without the import is not read.
func imapExpansions(fset *token.FileSet, name string, src []byte) ([]string, error) {
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	imports := false
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if p == imapModule || strings.HasPrefix(p, imapModule+"/") {
			imports = true
		}
	}
	if !imports {
		return nil, nil
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if what, bad := imapExpanders[sel.Sel.Name]; bad {
				p := fset.Position(sel.Sel.Pos())
				out = append(out, fmt.Sprintf("%s:%d %s", name, p.Line, what))
			}
		}
		return true
	})
	return out, nil
}

func TestNoCodeExpandsAnIMAPNumberSet(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var found []string
	read := 0
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
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(src), imapModule) {
			return nil
		}
		read++
		rel, _ := filepath.Rel(root, path)
		hits, err := imapExpansions(fset, filepath.ToSlash(rel), src)
		found = append(found, hits...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if read == 0 {
		t.Fatal("no server file imports go-imap: the walk is looking in the wrong place")
	}
	sort.Strings(found)
	if len(found) > 0 {
		t.Fatalf("go-imap expands a number set without bound (a range ending at 4294967295 never ends); walk its ranges instead (see lowestAbove):\n%s", strings.Join(found, "\n"))
	}
}

func TestTheIMAPExpansionGuardSeesEveryExpander(t *testing.T) {
	fset := token.NewFileSet()
	src := `package p

import "github.com/emersion/go-imap/v2"

func f(d *imap.SearchData, u imap.UIDSet, s *imap.SeqSet) {
	_ = d.AllUIDs()
	_ = d.AllSeqNums()
	_, _ = s.Nums()
	nums := u.Nums
	_, _ = nums()
}
`
	got, err := imapExpansions(fset, "p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("the guard saw %d of 4 expanders: %v", len(got), got)
	}
	plain := `package p

func f(s interface{ Nums() []int }) { _ = s.Nums() }
`
	got, err = imapExpansions(fset, "q.go", []byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a file without go-imap is not the guard's: %v", got)
	}
}
