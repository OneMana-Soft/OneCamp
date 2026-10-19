package helpers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A request's files reach the graph only as new nodes.
//
// WHY. Every write that adds files (a message, a comment, a task, a project,
// a forward) sent the request's files as they came, and a file is a graph
// struct with a uid: naming another member's file, or a private doc, linked
// that node into the new message and wrote the request's fields over it
// (October 2026; tests/integration TestAForwardNeverWritesANodeTheRequestNames).
// dgraphStruct.NewAttachments keeps only the upload's own fields. This fails
// on a function that writes a request's files (a request's MediaObj,
// MediaObjects or Attachments, where the request is a parameter or variable
// of an adapter type) into a graph struct without first replacing them with
// NewAttachments of themselves. It baselines nothing.
func TestARequestsFilesReachTheGraphOnlyAsNewNodes(t *testing.T) {
	writes := 0
	for _, root := range []string{"../business", "../controllers"} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			hits, n := unsafeClientFileWrites(t, path, src)
			writes += n
			for _, h := range hits {
				t.Errorf("%s writes a request's files into the graph as they came; replace them with dgraphStruct.NewAttachments of themselves first", h)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Forward, posts, post comments, chats (two), chat comments, doc and
	// task comments, tasks (two), projects.
	if writes < 11 {
		t.Fatalf("found %d writes of a request's files; the scan has stopped seeing them", writes)
	}
}

// The guard itself: an unchecked write is found, a checked one is not, and a
// field of something that isn't a request is left alone.
func TestUnsafeClientFileWritesFindsTheUncheckedOne(t *testing.T) {
	src := `package p

import (
	adapter "x/adapter/Post"
	dgraphStruct "x/models/dgraph"
)

func bad(in *adapter.Input) { _ = dgraphStruct.DgraphPost{MediaObj: in.MediaObj} }

func good(in *adapter.Input, by string) {
	in.MediaObj = dgraphStruct.NewAttachments(in.MediaObj, by, now)
	_ = dgraphStruct.DgraphPost{MediaObj: in.MediaObj}
}

func late(in adapter.Input) {
	p := dgraphStruct.DgraphPost{}
	p.MediaObj = in.MediaObj
	in.MediaObj = dgraphStruct.NewAttachments(in.MediaObj, "", now)
}

func copied(in *adapter.Input) { _ = &adapter.Other{MediaObj: in.MediaObj} }

func stored(p *dgraphStruct.DgraphPost) { _ = dgraphStruct.DgraphPost{MediaObj: p.MediaObj} }

func local() {
	var in adapter.Input
	_ = &dgraphStruct.DgraphTask{Attachments: in.Attachments}
}
`
	hits, n := unsafeClientFileWrites(t, "p.go", []byte(src))
	if n != 4 {
		t.Errorf("counted %d writes of a request's files, want 4 (bad, good, late, local)", n)
	}
	var fns []string
	for _, h := range hits {
		fns = append(fns, h[strings.LastIndex(h, " ")+1:])
	}
	if strings.Join(fns, ",") != "bad,late,local" {
		t.Errorf("found %v, want bad, late and local", hits)
	}
}

// clientFileFields are the request fields that carry files.
var clientFileFields = map[string]bool{"MediaObj": true, "MediaObjects": true, "Attachments": true}

// unsafeClientFileWrites is each write of a request's files into a graph
// struct (a composite literal's field, or an assignment to a field) that no
// earlier NewAttachments of the same field covers, as "file:line in func",
// and how many such writes there are in all.
func unsafeClientFileWrites(t *testing.T, path string, src []byte) (hits []string, writes int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	adapterPkgs := map[string]bool{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if !strings.Contains(p, "/adapter/") {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		adapterPkgs[name] = true
	}
	if len(adapterPkgs) == 0 {
		return nil, 0
	}
	isAdapterType := func(e ast.Expr) bool {
		if s, ok := e.(*ast.StarExpr); ok {
			e = s.X
		}
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && adapterPkgs[pkg.Name]
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		requests := map[string]bool{}
		for _, fl := range fd.Type.Params.List {
			if isAdapterType(fl.Type) {
				for _, n := range fl.Names {
					requests[n.Name] = true
				}
			}
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if vs, ok := n.(*ast.ValueSpec); ok && vs.Type != nil && isAdapterType(vs.Type) {
				for _, n := range vs.Names {
					requests[n.Name] = true
				}
			}
			return true
		})
		if len(requests) == 0 {
			continue
		}
		// requestFiles is "in.MediaObj" when e is a request's files.
		requestFiles := func(e ast.Expr) string {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok || !clientFileFields[sel.Sel.Name] {
				return ""
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || !requests[id.Name] {
				return ""
			}
			return id.Name + "." + sel.Sel.Name
		}
		cleaned := map[string]token.Pos{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			key := requestFiles(as.Lhs[0])
			call, ok := as.Rhs[0].(*ast.CallExpr)
			if key == "" || !ok || len(call.Args) == 0 {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewAttachments" && requestFiles(call.Args[0]) == key {
				if _, seen := cleaned[key]; !seen {
					cleaned[key] = as.Pos()
				}
			}
			return true
		})
		check := func(e ast.Expr) {
			key := requestFiles(e)
			if key == "" {
				return
			}
			writes++
			if at, ok := cleaned[key]; !ok || at > e.Pos() {
				hits = append(hits, fset.Position(e.Pos()).String()+" in "+fd.Name.Name)
			}
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				// Copying into another request isn't a write: that request
				// is cleaned where it is written.
				if x.Type != nil && isAdapterType(x.Type) {
					return true
				}
				for _, el := range x.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && clientFileFields[k.Name] {
							check(kv.Value)
						}
					}
				}
			case *ast.AssignStmt:
				if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
					if sel, ok := x.Lhs[0].(*ast.SelectorExpr); ok && clientFileFields[sel.Sel.Name] && requestFiles(x.Lhs[0]) == "" {
						check(x.Rhs[0])
					}
				}
			}
			return true
		})
	}
	return hits, writes
}
