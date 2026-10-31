package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The doc tree's node kinds are folder, file and link, written only as the
// constants domain/Doc names them (docNodeKind.go), and a page writes none:
// nothing writes "", "page" or a kind of its own.
//
// WHY. doc_node_kind decides what a node is everywhere: the editor opens only
// a node without one, every scan across docs takes only those (pageOnly), and
// only a page or a folder holds children. A kind spelled by hand ("Folder",
// "page", "") would make a node none of these, or a page that a page-only read
// then treats as a page with no body. domain/Doc's CreateDocNodes checks each
// kind before it writes (CheckNodeKind); this checks the source, so a write
// that goes round it fails here first. It reads every non-test Go file under
// business, controllers, domain, models and services:
//   - a NodeKind written in a composite literal or an assignment must be
//     NodeFolder, NodeFile or NodeLink (domain/Doc's), or another node's
//     NodeKind (a copy of a node read from the graph);
//   - no string writes doc_node_kind as an N-Quad ("<doc_node_kind>"), and no
//     map is keyed "doc_node_kind": the predicate is written through
//     DgraphDoc's field alone, which omits it for a page.
//
// It baselines nothing.
func TestNothingWritesANodeKindOutsideTheSet(t *testing.T) {
	var hits []string
	files := 0
	for _, root := range []string{"../business", "../controllers", "../domain", "../models", "../services"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files++
			hits = append(hits, nodeKindWrites(t, filepath.ToSlash(path), src)...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 1000 {
		t.Fatalf("read %d files; the guard is no longer looking at the source", files)
	}
	for _, h := range hits {
		t.Errorf("%s: a node's kind is written as one of domain/Doc's NodeFolder, NodeFile or NodeLink (or copied from a node read), and never as an N-Quad or map key", h)
	}
}

// The guard itself: it finds a kind spelled by hand, an empty one, one from a
// variable, an N-Quad and a map key, and leaves the constants, a copy and a
// read alone.
func TestNodeKindWritesFindsThem(t *testing.T) {
	src := `package x
const q = "{ docs(func: has(doc_uuid)) @filter(eq(doc_node_kind, \"folder\")) { doc_node_kind } }"
func f(in string, src *Doc) {
	a := Doc{NodeKind: docDomain.NodeFolder}
	b := &Doc{NodeKind: NodeLink, Title: "t"}
	c := Doc{NodeKind: src.NodeKind}
	b.NodeKind = docDomain.NodeFile
	_ = Doc{NodeKind: "page"}
	_ = Doc{NodeKind: ""}
	_ = Doc{NodeKind: in}
	c.NodeKind = "Folder"
	_ = "<0x1> <doc_node_kind> \"folder\" ."
	_ = map[string]any{"doc_node_kind": "link"}
	_, _, _ = a, b, c
}`
	got := nodeKindWrites(t, "x.go", []byte(src))
	want := []string{
		`x.go:8: NodeKind = "page"`,
		`x.go:9: NodeKind = ""`,
		`x.go:10: NodeKind = in`,
		`x.go:11: NodeKind = "Folder"`,
		`x.go:12: an N-Quad writes doc_node_kind`,
		`x.go:13: a map is keyed doc_node_kind`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("found\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// nodeKindConstants are the names a written kind may take.
var nodeKindConstants = map[string]bool{"NodeFolder": true, "NodeFile": true, "NodeLink": true}

// nodeKindWrites is every write of a node's kind in src that the rule above
// refuses, as "file:line: what".
func nodeKindWrites(t *testing.T, path string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var out []string
	at := func(n ast.Node, what string) {
		out = append(out, path+":"+strconv.Itoa(fset.Position(n.Pos()).Line)+": "+what)
	}
	allowed := func(v ast.Expr) bool {
		switch e := v.(type) {
		case *ast.Ident:
			return nodeKindConstants[e.Name]
		case *ast.SelectorExpr:
			return nodeKindConstants[e.Sel.Name] || e.Sel.Name == "NodeKind"
		}
		return false
	}
	text := func(v ast.Expr) string {
		switch e := v.(type) {
		case *ast.BasicLit:
			return e.Value
		case *ast.Ident:
			return e.Name
		}
		return "an expression"
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.KeyValueExpr:
			if k, ok := x.Key.(*ast.Ident); ok && k.Name == "NodeKind" && !allowed(x.Value) {
				at(x, "NodeKind = "+text(x.Value))
			}
			if k, ok := x.Key.(*ast.BasicLit); ok && k.Kind == token.STRING && strings.Trim(k.Value, "`\"") == "doc_node_kind" {
				at(x, "a map is keyed doc_node_kind")
			}
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				if s, ok := lhs.(*ast.SelectorExpr); ok && s.Sel.Name == "NodeKind" && i < len(x.Rhs) && !allowed(x.Rhs[i]) {
					at(x, "NodeKind = "+text(x.Rhs[i]))
				}
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING && strings.Contains(x.Value, "<doc_node_kind>") {
				at(x, "an N-Quad writes doc_node_kind")
			}
		}
		return true
	})
	return out
}

// No read starts at the node kind (docs F1, DGRAPH-LEAD-2's ask).
//
// WHY. doc_node_kind's exact index lists every folder, file and link in the
// workspace under three keys, and the files alone can pass 100,000: a read
// that starts there (func: eq(doc_node_kind, "file"), or has(doc_node_kind))
// walks all of them to find one folder's few. A folder's contents and its
// count start at the folder and go down its children edge (~doc_parent), as
// the budget line docs.folder measures (tests/integration), and the kind is
// only ever an @filter: pageOnly, LeafFilter, a check on a row read. This
// reads every non-test Go file under business, cmd, controllers, domain,
// helpers, models and services for a root on the kind:
//   - a string with func: and then a function of doc_node_kind;
//   - a root built from the kind's filters (pageOnly, PageOnly, LeafFilter,
//     a tree schema's Leaf), joined on with + or put in by a %s;
//   - doc_node_kind put in after func: by a %s.
//
// It baselines nothing.
func TestNoReadStartsAtTheNodeKind(t *testing.T) {
	var hits []string
	files := 0
	for _, root := range []string{"../business", "../cmd", "../controllers", "../domain", "../helpers", "../models", "../services"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files++
			hits = append(hits, kindRoots(t, filepath.ToSlash(path), src)...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 1000 {
		t.Fatalf("read %d files; the guard is no longer looking at the source", files)
	}
	for _, h := range hits {
		t.Errorf("%s: start the read at the parent (or the node, by uuid) and filter by kind; doc_node_kind's index holds every folder, file and link", h)
	}
}

// The guard itself: it finds a root on the kind spelled in a string, built
// with + and with a %s, and leaves the kind alone as a filter or a field.
func TestKindRootsFindsThem(t *testing.T) {
	src := "package x\n" +
		"func f(s Schema) {\n" +
		"	_ = `{ files(func: eq(doc_node_kind, \"file\")) { uid } }`\n" +
		"	_ = \"{ nodes(func:has( doc_node_kind )) { uid } }\"\n" +
		"	_ = \"{ leaves(func: \" + docDomain.LeafFilter + \") { uid } }\"\n" +
		"	_ = fmt.Sprintf(\"{ pages(func: %s) { uid } }\", pageOnly)\n" +
		"	_ = fmt.Sprintf(\"{ k(func: eq(%s, \\\"folder\\\")) { uid } }\", \"doc_node_kind\")\n" +
		"	_ = fmt.Sprintf(\"{ l(func: %s) }\", s.Leaf)\n" +
		"	_ = `{ kids(func: eq(doc_uuid, $id)) @filter(eq(doc_node_kind, \"folder\")) { doc_node_kind } }`\n" +
		"	_ = \"{ d(func: uid($id)) @filter(\" + LeafFilter + \") { uid } }\"\n" +
		"	_ = fmt.Sprintf(\"leaf(func: eq(%s, $id)) @filter(%s) { uid }\", s.ID, s.Leaf)\n" +
		"	_ = \"{ d(func: eq(doc_uuid, $id)) @filter(\" + PageOnly + \") }\"\n" +
		"}\n"
	got := kindRoots(t, "x.go", []byte(src))
	want := []string{
		`x.go:3: a read starts at doc_node_kind`,
		`x.go:4: a read starts at doc_node_kind`,
		`x.go:5: a read starts at the kind's filter LeafFilter`,
		`x.go:6: a read starts at the kind's filter pageOnly`,
		`x.go:7: a read starts at doc_node_kind`,
		`x.go:8: a read starts at the kind's filter Leaf`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("found\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

var (
	// kindRootRE is a root on the kind, written out.
	kindRootRE = regexp.MustCompile(`func:\s*[a-z_]+\(\s*doc_node_kind\b`)
	// rootEndRE is a string that ends where a root's function goes.
	rootEndRE = regexp.MustCompile(`func:\s*$`)
	// rootArgRE is a string that ends where a root function's predicate goes.
	rootArgRE = regexp.MustCompile(`func:\s*[a-z_]+\(\s*$`)
	// kindFilters are the names the kind's filters go by.
	kindFilters = map[string]bool{"pageOnly": true, "PageOnly": true, "LeafFilter": true, "Leaf": true}
	// formatVerbRE is one verb of a format string, or an escaped %.
	formatVerbRE = regexp.MustCompile(`%[-+# 0-9.*\[\]]*[a-zA-Z%]`)
)

// kindRoots is every read in src that starts at the node kind, as
// "file:line: what".
func kindRoots(t *testing.T, path string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var out []string
	at := func(n ast.Node, what string) {
		out = append(out, path+":"+strconv.Itoa(fset.Position(n.Pos()).Line)+": "+what)
	}
	str := func(e ast.Expr) (string, bool) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(lit.Value)
		return v, err == nil
	}
	name := func(e ast.Expr) string {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name
		case *ast.SelectorExpr:
			return x.Sel.Name
		}
		return ""
	}
	// last is the string a + chain's left side ends with.
	var last func(ast.Expr) (string, bool)
	last = func(e ast.Expr) (string, bool) {
		if b, ok := e.(*ast.BinaryExpr); ok && b.Op == token.ADD {
			return last(b.Y)
		}
		return str(e)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if v, ok := str(x); ok && kindRootRE.MatchString(v) {
				at(x, "a read starts at doc_node_kind")
			}
		case *ast.BinaryExpr:
			if v, ok := last(x.X); ok && x.Op == token.ADD && rootEndRE.MatchString(v) && kindFilters[name(x.Y)] {
				at(x, "a read starts at the kind's filter "+name(x.Y))
			}
		case *ast.CallExpr:
			if name(x.Fun) != "Sprintf" || len(x.Args) < 2 {
				return true
			}
			format, ok := str(x.Args[0])
			if !ok {
				return true
			}
			arg := 1
			for _, loc := range formatVerbRE.FindAllStringIndex(format, -1) {
				if format[loc[1]-1] == '%' {
					continue
				}
				if arg >= len(x.Args) {
					break
				}
				before := format[:loc[0]]
				if rootEndRE.MatchString(before) && kindFilters[name(x.Args[arg])] {
					at(x, "a read starts at the kind's filter "+name(x.Args[arg]))
				}
				if v, ok := str(x.Args[arg]); ok && v == "doc_node_kind" && rootArgRE.MatchString(before) {
					at(x, "a read starts at doc_node_kind")
				}
				arg++
			}
		}
		return true
	})
	return out
}
