package helpers

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every line the server logs goes through the one handler loggerInit builds
// (its TeeHandler), which takes each URL's query off before the line reaches
// stdout or otel_logs (TestALogLineReachesNeitherSinkWithAURLQuery). A logger
// or a handler built anywhere else, or a default pointed somewhere else, would
// log around it.
func TestEveryLogLineGoesThroughTheOneHandler(t *testing.T) {
	fset := token.NewFileSet()
	var hits []string
	for _, path := range productionGoFiles(t) {
		if filepath.ToSlash(path) == "../initializers/loggerInit/loggerInit.go" {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		hits = append(hits, loggersBuilt(fset, file)...)
	}
	if len(hits) > 0 {
		t.Errorf("%d places outside initializers/loggerInit build a logger or a handler, or point one somewhere else; log through helpers' Log* instead:\n%s", len(hits), strings.Join(hits, "\n"))
	}
}

func TestLoggersBuiltFindsEachWayToLogAroundTheHandler(t *testing.T) {
	files := []struct{ name, src string }{
		{"p.go", `package p

import (
	"log"
	stdlog "log"
	"log/slog"
	. "log/slog"
	"os"

	"github.com/akashc777/OneCamp/helpers"
	"go.opentelemetry.io/contrib/bridges/otelslog"
)

func f() {
	l := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(l)
	text := slog.NewTextHandler(os.Stderr, nil)
	errs := log.New(os.Stderr, "", 0)
	log.SetOutput(os.Stdout)
	bridged := slog.NewLogLogger(text, slog.LevelInfo)
	otel := otelslog.NewHandler("p")
	named := otelslog.NewLogger("p")
	aliased := stdlog.New(os.Stderr, "", 0)
	log.Default().SetOutput(os.Stderr)
	build := slog.New
	helpers.LogWarnWithContext(ctx, "went wrong: %v", err)
	log.Printf("ran %d", n)
	slog.Info("ran")
	fields := slog.String("k", "v")
}`},
		// A package a file calls log is the log package only if its path says
		// so.
		{"q.go", `package q

import "example.com/audit/log"

func g() {
	entry := log.New("signed in")
}`},
	}
	fset := token.NewFileSet()
	var got []string
	for _, f := range files {
		file, err := parser.ParseFile(fset, f.name, f.src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, loggersBuilt(fset, file)...)
	}
	want := []string{
		`p.go:7: . "log/slog"`,
		"p.go:15: slog.New",
		"p.go:15: slog.NewJSONHandler",
		"p.go:16: slog.SetDefault",
		"p.go:17: slog.NewTextHandler",
		"p.go:18: log.New",
		"p.go:19: log.SetOutput",
		"p.go:20: slog.NewLogLogger",
		"p.go:21: otelslog.NewHandler",
		"p.go:22: otelslog.NewLogger",
		"p.go:23: stdlog.New",
		"p.go:24: log.Default().SetOutput",
		"p.go:25: slog.New",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("found\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// loggerBuilders are, by import path, the functions that build a logger or a
// handler, or point the default logger somewhere else.
var loggerBuilders = map[string]map[string]bool{
	"log/slog": {"New": true, "NewJSONHandler": true, "NewTextHandler": true, "NewLogLogger": true, "SetDefault": true},
	"log":      {"New": true},
	"go.opentelemetry.io/contrib/bridges/otelslog": {"NewHandler": true, "NewLogger": true},
}

// loggersBuilt lists file's ways around the handler, one "file:line: use"
// each: a function in loggerBuilders, its package known by its import path
// whatever file calls it, called or taken as a value; any SetOutput, the log
// package's own or a logger's (log.Default().SetOutput); and a dot import of a
// package in loggerBuilders, whose calls name no package.
func loggersBuilt(fset *token.FileSet, file *ast.File) []string {
	paths := importPaths(file)
	var hits []string
	ast.Inspect(file, func(n ast.Node) bool {
		var use string
		switch n := n.(type) {
		case *ast.ImportSpec:
			if p, _ := strconv.Unquote(n.Path.Value); n.Name != nil && n.Name.Name == "." && loggerBuilders[p] != nil {
				use = ". " + n.Path.Value
			}
		case *ast.SelectorExpr:
			pkg, _ := n.X.(*ast.Ident)
			if n.Sel.Name == "SetOutput" || (pkg != nil && loggerBuilders[paths[pkg.Name]][n.Sel.Name]) {
				use = types.ExprString(n)
			}
		}
		if use != "" {
			at := fset.Position(n.Pos())
			hits = append(hits, fmt.Sprintf("%s:%d: %s", at.Filename, at.Line, use))
		}
		return true
	})
	return hits
}
