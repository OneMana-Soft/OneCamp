package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// handQuotedAllowed are the domain files that quote something that is not a
// uuid list, by file and why.
var handQuotedAllowed = map[string]string{
	"Chat/groupingLookup.go":   "grouping ids are two uuids with a space, or 32 hex characters: not uuids, checked by isGroupingID",
	"Webhook/webhookDomain.go": "a JSON string passed as a query parameter, never written into query text",
}

// Domain code writes a DQL list of ids with dgraphquery.UUIDList (or
// QuotedUUIDs, UUIDListOf), never by quoting each id by hand.
//
// WHY. About ten domain functions built `"` + id + `"` lists themselves. Some
// parsed each id, one passed anything made of letters, digits and dashes, one
// a regexp; some wrote an upper-case id as it came, which the graph (lower
// case) never matches, and none dropped repeats. UUIDList is the one rule:
// uuids only, once, lower case, so nothing from a request reaches the query
// unchecked. This reads every domain file for a string built as `"` + x + `"`.
func TestDomainWritesUUIDListsThroughDgraphquery(t *testing.T) {
	read := 0
	err := filepath.WalkDir("../domain", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel("../domain", path)
		rel = filepath.ToSlash(rel)
		read++
		if _, ok := handQuotedAllowed[rel]; ok {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		isQuote := func(e ast.Expr) bool {
			lit, ok := e.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return false
			}
			s, err := strconv.Unquote(lit.Value)
			return err == nil && s == `"`
		}
		ast.Inspect(f, func(n ast.Node) bool {
			b, ok := n.(*ast.BinaryExpr)
			if !ok || b.Op != token.ADD || !isQuote(b.Y) {
				return true
			}
			left := b.X
			for {
				inner, ok := left.(*ast.BinaryExpr)
				if !ok || inner.Op != token.ADD {
					break
				}
				left = inner.X
			}
			if isQuote(left) {
				t.Errorf("%s: quotes an id by hand; write the list with dgraphquery.UUIDList", fset.Position(b.Pos()))
				return false
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if read < 80 {
		t.Fatalf("read %d domain files; the guard is no longer looking at them", read)
	}
	for rel := range handQuotedAllowed {
		if _, err := os.Stat("../domain/" + rel); err != nil {
			t.Errorf("%s is allowed but gone; take it off handQuotedAllowed", rel)
		}
	}
}
