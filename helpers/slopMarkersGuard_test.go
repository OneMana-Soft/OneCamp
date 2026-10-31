package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"
	"testing"
)

// No new slop in production Go (owner, 22 Oct 2026: "make sure we are not
// shipping AI slop or useless code"; design/LEAD-RULES-2026-10-21.md, "No
// slop"). Each file's markers are counted and held to
// testdata/slop-markers-baseline.txt, one "path marker count" line each:
//
//   - commented-code: a comment that is Go statements, code left switched off.
//     A doc comment's example is indented (gofmt's code block) and isn't one.
//   - todo-no-owner: a TODO, FIXME, XXX or HACK with no owner, TODO(name).
//   - nolint-no-reason: a //nolint with no "// why" after it.
//   - print: fmt.Print*, log.Print* or println outside a command: logs go
//     through helpers.Log*, which carry the request and reach the log store.
//   - empty-err: an `if err != nil {}` with nothing in it, not even a comment
//     saying why the error is let go.
//
// The line a marker sits on isn't kept, only the count, so editing around one
// never moves it. A count that falls fails too, until the baseline is lowered
// with it (WRITE_SLOP_BASELINE=1), so progress shows in the diff.
const slopBaselinePath = "testdata/slop-markers-baseline.txt"

const slopBaselineHeader = `# Slop markers in production Go, per file: helpers/slopMarkersGuard_test.go.
# A line is debt: lower it when you clear one (WRITE_SLOP_BASELINE=1 rewrites this file).
# A new line needs a reason in its commit, never a habit.
`

var (
	slopTodo        = regexp.MustCompile(`\b(TODO|FIXME|XXX|HACK)\b`)
	slopTodoOwned   = regexp.MustCompile(`\b(TODO|FIXME|XXX|HACK)\([^)\s]+\)`)
	slopNolint      = regexp.MustCompile(`^//\s*nolint`)
	slopNolintWhy   = regexp.MustCompile(`^//\s*nolint(:\S+)?\s*//\s*\S`)
	slopPunctuation = regexp.MustCompile(`[(){}=;]`)
	// A word, a space and "(": gofmt never writes that after a name it calls,
	// so "seconds (context)" is prose. A keyword may come before one.
	slopProseParen = regexp.MustCompile(`(^|[^A-Za-z0-9_.])([A-Za-z_][A-Za-z0-9_]*) \(`)
	slopKeywords   = map[string]bool{"if": true, "for": true, "switch": true, "return": true, "go": true, "defer": true,
		"func": true, "case": true, "select": true, "range": true, "else": true, "chan": true, "map": true, "interface": true, "struct": true}
)

func TestNoNewSlopInProductionGo(t *testing.T) {
	found := map[string]int{}
	files := productionGoFiles(t)
	if len(files) < 1000 {
		t.Fatalf("scanned %d files: the check is no longer reading the server's code", len(files))
	}
	for _, path := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			continue // a file that doesn't parse fails the build, not this
		}
		rel := strings.TrimPrefix(path, "../")
		for marker, n := range slopIn(f) {
			found[rel+" "+marker] += n
		}
	}
	checkRatchet(t, found, slopBaselinePath, slopBaselineHeader, "WRITE_SLOP_BASELINE",
		"Fix them rather than list them: delete code left commented out (git keeps it), give a TODO its owner, "+
			"give a nolint its reason, log through helpers.Log*, and handle an error or say in the empty branch why it's let go.")
}

// slopIn counts a parsed file's markers.
func slopIn(f *ast.File) map[string]int {
	out := map[string]int{}
	for _, cg := range f.Comments {
		var run []string
		flush := func() {
			if len(run) > 0 && slopPunctuation.MatchString(strings.Join(run, " ")) && commentedCode(run) {
				out["commented-code"]++
			}
			run = nil
		}
		for _, c := range cg.List {
			if slopTodo.MatchString(c.Text) && !slopTodoOwned.MatchString(c.Text) {
				out["todo-no-owner"]++
			}
			if slopNolint.MatchString(c.Text) && !slopNolintWhy.MatchString(c.Text) {
				out["nolint-no-reason"]++
			}
			body, line := strings.CutPrefix(c.Text, "//")
			// A blank line ends a run; a block comment, a doc comment's indented
			// example and a directive (//go:, //nolint) are never code left off.
			if !line || strings.TrimSpace(body) == "" || !strings.HasPrefix(body, " ") || strings.HasPrefix(body, "  ") {
				flush()
				continue
			}
			run = append(run, body[1:])
		}
		flush()
	}
	if f.Name.Name != "main" {
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && isPrint(call) {
				out["print"]++
			}
			return true
		})
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if s, ok := n.(*ast.IfStmt); ok && isEmptyErrBranch(s, f) {
			out["empty-err"]++
		}
		return true
	})
	return out
}

func isPrint(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		pkg, ok := fn.X.(*ast.Ident)
		return ok && (pkg.Name == "fmt" || pkg.Name == "log") && strings.HasPrefix(fn.Sel.Name, "Print")
	case *ast.Ident:
		return fn.Name == "println" || fn.Name == "print"
	}
	return false
}

// isEmptyErrBranch is `if err != nil {}` (any name ending in err) with
// nothing in it, not even a comment.
func isEmptyErrBranch(s *ast.IfStmt, f *ast.File) bool {
	be, ok := s.Cond.(*ast.BinaryExpr)
	if !ok || be.Op != token.NEQ || s.Else != nil || len(s.Body.List) > 0 {
		return false
	}
	x, xok := be.X.(*ast.Ident)
	y, yok := be.Y.(*ast.Ident)
	if !xok || !yok || y.Name != "nil" || !strings.HasSuffix(strings.ToLower(x.Name), "err") {
		return false
	}
	for _, cg := range f.Comments {
		if cg.Pos() > s.Body.Lbrace && cg.End() < s.Body.Rbrace {
			return false
		}
	}
	return true
}

// commentedCode is whether lines (a comment's text) are Go statements, at
// least one shaped like code rather than prose that happens to parse:
// "return early", "(userUUID)", "resolved == false" all parse.
func commentedCode(lines []string) bool {
	for _, l := range lines {
		for _, m := range slopProseParen.FindAllStringSubmatch(l, -1) {
			if !slopKeywords[m[2]] {
				return false
			}
		}
	}
	src := "package p\nfunc _() {\n" + strings.Join(lines, "\n") + "\n}\n"
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	if err != nil {
		return false
	}
	for _, st := range f.Decls[0].(*ast.FuncDecl).Body.List {
		if codeShaped(st) {
			return true
		}
	}
	return false
}

func codeShaped(st ast.Stmt) bool {
	switch s := st.(type) {
	case *ast.LabeledStmt:
		return codeShaped(s.Stmt)
	case *ast.AssignStmt:
		for _, l := range s.Lhs {
			switch l.(type) {
			case *ast.Ident, *ast.SelectorExpr, *ast.IndexExpr, *ast.StarExpr:
			default:
				return false
			}
		}
		return true
	case *ast.IncDecStmt, *ast.DeclStmt, *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt,
		*ast.TypeSwitchStmt, *ast.GoStmt, *ast.DeferStmt, *ast.SelectStmt, *ast.SendStmt:
		return true
	case *ast.ReturnStmt:
		for _, r := range s.Results {
			if _, plain := r.(*ast.Ident); !plain {
				return true
			}
		}
		return len(s.Results) > 1
	case *ast.ExprStmt:
		call, ok := s.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		switch call.Fun.(type) {
		case *ast.SelectorExpr:
			return true
		case *ast.Ident:
			return len(call.Args) > 0
		}
	}
	return false
}

// The markers' rules, on the cases that shaped them.
func TestSlopMarkersReadCodeNotProse(t *testing.T) {
	cases := []struct {
		src  string
		want map[string]int
	}{
		{"package p\n// x := compute(a)\n// if x > 0 {\n// \treturn x\n// }\nvar v int\n", map[string]int{"commented-code": 1}},
		{"package p\n// helpers.LogErrorWithContext(ctx, \"x %v\", err)\nvar v int\n", map[string]int{"commented-code": 1}},
		{"package p\n// return early when the list is empty (userUUID)\n// resolved == false\n// seconds (context)\nvar v int\n", nil},
		{"package p\n// Example:\n//\n//\tx := f(a)\nvar v int\n", nil},
		{"package p\n// TODO: tidy\n// TODO(akash): tidy\n// FIXME(sam) later\nvar v int\n", map[string]int{"todo-no-owner": 1}},
		{"package p\nvar v = 1 //nolint:gosec\nvar w = 1 //nolint:gosec // fixed input\n", map[string]int{"nolint-no-reason": 1}},
		{"package p\nimport \"fmt\"\nfunc f() { fmt.Println(1) }\n", map[string]int{"print": 1}},
		{"package main\nimport \"fmt\"\nfunc main() { fmt.Println(1) }\n", nil},
		{"package p\nfunc f(err error) {\n\tif err != nil {\n\t}\n\tif err != nil {\n\t\t// best effort: a cache miss\n\t}\n}\n", map[string]int{"empty-err": 1}},
	}
	for i, c := range cases {
		f, err := parser.ParseFile(token.NewFileSet(), "", c.src, parser.ParseComments)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		got := slopIn(f)
		if len(got) != len(c.want) {
			t.Errorf("case %d: got %v, want %v", i, got, c.want)
			continue
		}
		for k, n := range c.want {
			if got[k] != n {
				t.Errorf("case %d: %s = %d, want %d (all: %v)", i, k, got[k], n, got)
			}
		}
	}
}
