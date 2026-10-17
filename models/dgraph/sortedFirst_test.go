package models

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Dgraph returns at most 1,000 results from a sorted block (orderasc or
// orderdesc) that names no first, and says nothing (helpers/dgraphquery
// SortedCap). On 14 Oct 2026 that cut board columns, My Tasks, "show all"
// lists and task histories at 1,000. This reads the backend's query text and
// checks that each block that sorts also names its first.
//
// Query text is read as the program builds it, as far as that is known before
// it runs: a string, a chain of strings and constants joined with +, and a
// fmt.Sprintf or Fprintf whose format and arguments are constants, in this
// package or another. A block built from a sort constant
// ("(" + emojiStatusOrder + ")") was invisible when only single strings were
// read, and two such blocks reached beta that way on 15 Oct 2026. What is
// known only at run time (a variable, a call) reads as a blank, so a sort key
// handed to a function that pages it ("orderdesc: task_due_date", outside any
// parentheses) is not checked here; that function's own block is.
func TestEverySortedBlockNamesItsFirst(t *testing.T) {
	src := loadQuerySource(t, "../..", "domain", "models", "business", "controllers", "services", "helpers")
	unbounded, checked := unboundedSortedBlocks(src.queryTexts())
	for _, b := range unbounded {
		t.Errorf("a sorted block with no first stops at 1,000 results: %s", b)
	}
	if checked < 40 {
		t.Fatalf("checked only %d sorted blocks; the scan has stopped seeing them", checked)
	}
}

// The scan sees blocks built from constants, in this package or another and
// in a function, and by Sprintf; a block whose sort is known only at run time
// is not checked.
func TestSortedBlocksBuiltFromConstantsAreSeen(t *testing.T) {
	dir := t.TempDir()
	src := map[string]string{
		"go.mod": "module example.test/q\n",
		"a/a.go": `package a

import (
	"fmt"

	"example.test/q/b"
)

const byName = "orderasc: name"

func Q(key string, n int) []string {
	const local = "orderdesc: at"
	return []string{
		"x (" + byName + ") {",
		fmt.Sprintf("y (%s, offset: %d) {", local, n),
		"z (" + b.ByAge + ", first: 5) {",
		fmt.Sprintf("w (%s, first: %d) {", key, n),
		fmt.Sprintf("v (%s) {", key),
	}
}
`,
		"b/b.go": "package b\n\nconst ByAge = \"orderdesc: age\"\n",
	}
	for name, body := range src {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	unbounded, checked := unboundedSortedBlocks(loadQuerySource(t, dir, "a", "b").queryTexts())
	var got []string
	for _, b := range unbounded {
		got = append(got, b[strings.Index(b, ": (")+2:])
	}
	want := []string{"(orderasc: name)", "(orderdesc: at, offset: …)"}
	if strings.Join(got, "|") != strings.Join(want, "|") || checked != 3 {
		t.Fatalf("%d checked, unbounded %q; want 3 checked, unbounded %q", checked, got, want)
	}
}

// unboundedSortedBlocks is each sorted block in texts that names no first,
// as "<where>: (<its arguments>)", sorted; and how many sorted blocks there
// were.
func unboundedSortedBlocks(texts []queryText) (unbounded []string, checked int) {
	order := regexp.MustCompile(`order(asc|desc)\s*:`)
	first := regexp.MustCompile(`\bfirst\s*:`)
	seen := map[string]bool{}
	for _, text := range texts {
		for _, m := range order.FindAllStringIndex(text.s, -1) {
			group, ok := enclosingGroup(text.s, m[0])
			if !ok {
				continue
			}
			checked++
			if b := text.pos + ": (" + strings.ReplaceAll(group, unknownText, "…") + ")"; !first.MatchString(group) && !seen[b] {
				seen[b] = true
				unbounded = append(unbounded, b)
			}
		}
	}
	sort.Strings(unbounded)
	return unbounded, checked
}

// unknownText stands in a query text for what is known only at run time.
const unknownText = "\x00"

// querySource is the Go source of the packages under some directories, with
// their constants, so that query text built from constants can be read.
type querySource struct {
	t     *testing.T
	fset  *token.FileSet
	pkgs  map[string]*queryPkg // by import path
	files []*queryFile
}

type queryPkg struct {
	name   string
	consts map[string]queryConst
}

type queryConst struct {
	expr ast.Expr
	file *queryFile
}

type queryFile struct {
	pkg     *queryPkg
	ast     *ast.File
	imports map[string]string // the name a file calls a package by → its import path
}

// queryText is one piece of text a query may be built from, folded, and where
// it is in the source.
type queryText struct {
	s   string
	pos string
}

// loadQuerySource parses the non-test Go files under dirs of the module whose
// go.mod is in root.
func loadQuerySource(t *testing.T, root string, dirs ...string) *querySource {
	t.Helper()
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	module := ""
	for _, line := range strings.Split(string(mod), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
			module = f[1]
		}
	}
	src := &querySource{t: t, fset: token.NewFileSet(), pkgs: map[string]*queryPkg{}}
	for _, d := range dirs {
		_ = filepath.WalkDir(filepath.Join(root, d), func(p string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(src.fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			rel, _ := filepath.Rel(root, filepath.Dir(p))
			path := module + "/" + filepath.ToSlash(rel)
			pkg := src.pkgs[path]
			if pkg == nil {
				pkg = &queryPkg{name: f.Name.Name, consts: map[string]queryConst{}}
				src.pkgs[path] = pkg
			}
			qf := &queryFile{pkg: pkg, ast: f}
			src.files = append(src.files, qf)
			for _, decl := range f.Decls {
				if g, ok := decl.(*ast.GenDecl); ok && g.Tok == token.CONST {
					addConsts(pkg.consts, g, qf)
				}
			}
			return nil
		})
	}
	// Imports are read once every package's name is known: a package is
	// called by its own name (models, Domain), not by the end of its path.
	for _, f := range src.files {
		f.imports = map[string]string{}
		for _, imp := range f.ast.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			name := path[strings.LastIndex(path, "/")+1:]
			if p := src.pkgs[path]; p != nil {
				name = p.name
			}
			if imp.Name != nil {
				name = imp.Name.Name
			}
			f.imports[name] = path
		}
	}
	return src
}

func addConsts(into map[string]queryConst, g *ast.GenDecl, f *queryFile) {
	for _, spec := range g.Specs {
		v, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range v.Names {
			if i < len(v.Values) {
				into[name.Name] = queryConst{expr: v.Values[i], file: f}
			}
		}
	}
}

// queryTexts folds every string, every chain of them joined with +, and every
// fmt.Sprintf or Fprintf in the source, each whole: a piece of a chain or a
// Sprintf's format is read only as part of it, as one alone may hold half a
// block.
func (src *querySource) queryTexts() []queryText {
	var out []queryText
	for _, f := range src.files {
		var stack []ast.Node
		locals := map[string]queryConst{}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			if n == nil {
				if _, ok := stack[len(stack)-1].(*ast.FuncDecl); ok {
					locals = map[string]queryConst{}
				}
				stack = stack[:len(stack)-1]
				return false
			}
			var parent ast.Node
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			stack = append(stack, n)
			if fn, ok := n.(*ast.FuncDecl); ok {
				// A function's own constants, for the strings inside it.
				locals = map[string]queryConst{}
				ast.Inspect(fn, func(m ast.Node) bool {
					if g, ok := m.(*ast.GenDecl); ok && g.Tok == token.CONST {
						addConsts(locals, g, f)
					}
					return true
				})
			}
			if src.partOfABiggerText(parent, f) {
				return true
			}
			switch e := n.(type) {
			case *ast.BasicLit:
				if e.Kind != token.STRING {
					return true
				}
			case *ast.BinaryExpr:
				if e.Op != token.ADD {
					return true
				}
			case *ast.ParenExpr:
			case *ast.CallExpr:
				if _, ok := src.formatArg(e, f); !ok {
					return true
				}
			default:
				return true
			}
			out = append(out, queryText{s: src.fold(n.(ast.Expr), f, locals, 0), pos: src.fset.Position(n.Pos()).String()})
			return true
		})
	}
	return out
}

// partOfABiggerText is whether a node under parent is folded as part of it.
func (src *querySource) partOfABiggerText(parent ast.Node, f *queryFile) bool {
	switch p := parent.(type) {
	case *ast.BinaryExpr:
		return p.Op == token.ADD
	case *ast.ParenExpr:
		return true
	case *ast.CallExpr:
		_, ok := src.formatArg(p, f)
		return ok
	}
	return false
}

// formatArg is the index of a fmt.Sprintf's or Fprintf's format among call's
// arguments.
func (src *querySource) formatArg(call *ast.CallExpr, f *queryFile) (int, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return 0, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || f.imports[pkg.Name] != "fmt" {
		return 0, false
	}
	switch sel.Sel.Name {
	case "Sprintf":
		return 0, len(call.Args) > 0
	case "Fprintf":
		return 1, len(call.Args) > 1
	}
	return 0, false
}

var verb = regexp.MustCompile(`%(\[(\d+)\])?[-+# 0]*\d*(\.\d+)?([a-zA-Z%])`)

// fold is e's value as text, with unknownText for what only running the
// program can tell.
func (src *querySource) fold(e ast.Expr, f *queryFile, locals map[string]queryConst, depth int) string {
	if depth > 500 {
		return unknownText
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			if s, err := strconv.Unquote(x.Value); err == nil {
				return s
			}
		}
		if x.Kind == token.INT {
			return x.Value
		}
	case *ast.ParenExpr:
		return src.fold(x.X, f, locals, depth+1)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return src.fold(x.X, f, locals, depth+1) + src.fold(x.Y, f, locals, depth+1)
		}
	case *ast.Ident:
		if c, ok := locals[x.Name]; ok {
			return src.fold(c.expr, c.file, locals, depth+1)
		}
		if c, ok := f.pkg.consts[x.Name]; ok {
			return src.fold(c.expr, c.file, nil, depth+1)
		}
	case *ast.SelectorExpr:
		if pkg, ok := x.X.(*ast.Ident); ok {
			if p := src.pkgs[f.imports[pkg.Name]]; p != nil {
				if c, ok := p.consts[x.Sel.Name]; ok {
					return src.fold(c.expr, c.file, nil, depth+1)
				}
			}
		}
	case *ast.CallExpr:
		at, ok := src.formatArg(x, f)
		if !ok {
			break
		}
		format := src.fold(x.Args[at], f, locals, depth+1)
		args := x.Args[at+1:]
		next := 0
		return verb.ReplaceAllStringFunc(format, func(v string) string {
			m := verb.FindStringSubmatch(v)
			if m[4] == "%" {
				return "%"
			}
			if m[2] != "" {
				next, _ = strconv.Atoi(m[2])
				next--
			}
			i := next
			next++
			if i < 0 || i >= len(args) {
				return unknownText
			}
			s := src.fold(args[i], f, locals, depth+1)
			if m[4] == "q" && !strings.Contains(s, unknownText) {
				return strconv.Quote(s)
			}
			return s
		})
	}
	return unknownText
}

// enclosingGroup is the text inside the parentheses around s[at], up to the
// matching one or the string's end; ok is false outside any.
func enclosingGroup(s string, at int) (string, bool) {
	depth, open := 0, -1
	for i := at - 1; i >= 0; i-- {
		switch s[i] {
		case ')':
			depth++
		case '(':
			if depth == 0 {
				open = i
			} else {
				depth--
			}
		}
		if open >= 0 {
			break
		}
	}
	if open < 0 {
		return "", false
	}
	depth = 0
	for i := open + 1; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return s[open+1 : i], true
			}
			depth--
		}
	}
	return s[open+1:], true
}
