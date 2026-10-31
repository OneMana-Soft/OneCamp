package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// majorVersion is a module path's last element when it only names a major
// version (…/go-redis/v9).
var majorVersion = regexp.MustCompile(`^v[0-9]+$`)

// importPaths maps each name file calls an imported package by to that
// package's import path, so a guard knows a package whatever a file calls it.
// An import that gives no name is called by the one Go's convention takes from
// its path: the last element, past a major version and without a "go-" prefix,
// up to the first character a name can't hold (…/redis/go-redis/v9 is redis,
// …/opensearch-go/v4 is opensearch). A blank or dot import gives no name to
// call by.
func importPaths(file *ast.File) map[string]string {
	paths := make(map[string]string, len(file.Imports))
	for _, im := range file.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		name := path.Base(p)
		if majorVersion.MatchString(name) {
			name = path.Base(path.Dir(p))
		}
		name = strings.TrimPrefix(name, "go-")
		if i := strings.IndexFunc(name, func(r rune) bool { return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) }); i >= 0 {
			name = name[:i]
		}
		if im.Name != nil {
			name = im.Name.Name
		}
		if name != "_" && name != "." {
			paths[name] = p
		}
	}
	return paths
}

func TestImportPathsNamesEachImportAsAFileCallsIt(t *testing.T) {
	const src = `package p

import (
	_ "embed"
	stdlog "log"
	"log/slog"
	. "strings"

	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/redis/go-redis/v9"
)
`
	file, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"stdlog":     "log",
		"slog":       "log/slog",
		"dgo":        "github.com/dgraph-io/dgo/v230",
		"api":        "github.com/dgraph-io/dgo/v230/protos/api",
		"opensearch": "github.com/opensearch-project/opensearch-go/v4",
		"redis":      "github.com/redis/go-redis/v9",
	}
	if got := importPaths(file); !maps.Equal(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}
