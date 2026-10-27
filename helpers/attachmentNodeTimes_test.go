package helpers

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every attachment node the server writes says when it was written.
//
// WHY. A deleted message's file stays while an item still there holds it
// (business/Attachment ArchiveFilesOf), and a node written before 19 Oct, or
// one with no time at all, holds whatever file it names: until then a write
// kept any file its request named, and some writes set no time. That is safe
// only while no write of today can make a
// node naming a file with no time, or with a time its request chose: such a
// node would keep a file open to the people of its place after every message
// showing it was deleted.
//
// This finds every attachment node a non-test file makes: a DgraphAttachment
// literal (an elided one in a list or a map of them too), a copy of one (*f),
// new(DgraphAttachment) and a var of the type. Each must be
//   - a new node with its time: it sets CreatedAt, and newNodeTimes names its
//     writer and where that time comes from;
//   - an update of nodes the graph holds: it sets Uid and doesn't name its
//     file (Uuid), so an upsert whose variable finds nothing makes a node that
//     names no file; or guardedUpdates names it and the condition, held where
//     it says, under which its mutation writes only the nodes its query finds;
//   - or never written as a node (notNodes says why).
//
// Every call of NewAttachments, which every member's write of files goes
// through (TestARequestsFilesReachTheGraphOnlyAsNewNodes), passes the time of
// its write: a time.Now() of the write, or helpers.CreateTimeOrNow, which is
// that unless a server import or the demo's seed backdates the context (only
// they call helpers.WithImportTimestamp, and they write no files with it), or
// a time newAttachmentTimes names with why no request sets it. A time passed
// in as a parameter is followed to the callers in its package. Each entry of
// these tables must still be found, so none outlives its writer. It
// baselines nothing.
func TestEveryAttachmentNodeTheServerWritesSaysWhenItWasWritten(t *testing.T) {
	files := map[string][]byte{}
	err := filepath.WalkDir("..", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch name := d.Name(); {
			case p == "..":
			case name == "tests", name == "testdata", name == "vendor", name == "node_modules", strings.HasPrefix(name, "."):
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(strings.TrimPrefix(p, "../"))] = src
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := scanAttachmentNodes(t, files)
	for _, p := range s.problems(newNodeTimes, guardedUpdates, notNodes, newAttachmentTimes, backdaters) {
		t.Error(p)
	}
	// NewAttachments, the generic and the Slack import, a conversation's
	// shared copies, the deleted message's stamp, a task's or project's file
	// let go, the trash's archive and restore, and a doc comment's shown
	// files; and NewAttachments' callers: posts, post comments, conversation
	// and group messages, their comments, doc and task comments, tasks (two),
	// projects and forwards.
	if len(s.nodes) < 9 || len(s.calls) < 11 {
		t.Fatalf("found %d attachment nodes and %d calls of NewAttachments; the scan has stopped seeing them", len(s.nodes), len(s.calls))
	}

	// A task comment's time can be a backfill's, which business/GitHub's sync
	// sets in Go. A request never sets it: a comment and its files take the
	// server's time.
	src, err := os.ReadFile("../adapter/Task/taskAdapter.go")
	if err != nil {
		t.Fatal(err)
	}
	if tag := fieldTag(t, src, "CreateOrUpdateTaskCommentInput", "CreatedAt"); tag != `json:"-"` {
		t.Errorf("adapter/Task CreateOrUpdateTaskCommentInput.CreatedAt is tagged %s: a request sets a task comment's time, and its files'; tag it json:\"-\"", tag)
	}
}

// newNodeTimes are the writers of new attachment nodes (file::function), and
// where the time each sets comes from: every source of it, a parameter no
// caller in its package passes as "param:<name>", sorted and joined by " | ".
var newNodeTimes = map[string]struct{ from, why string }{
	"models/dgraph/newAttachments.go::NewAttachments": {"param:at",
		"its callers' time, each checked below"},
	"business/Import/attachment_worker.go::processAttachmentChunk": {"time.Now()",
		"the import's write"},
	"business/SlackImport/file_worker.go::processFileChunk": {"slackTsToTime(meta.ParentTs) | time.Now()",
		"the export's message time, or the write's when it has none: never zero; not the write's own, so business/Attachment holds takes an imported file's node as no older than the row the import made"},
	"business/Conversation/share.go::copiedFiles": {"m.At | r.At",
		"the shared message's time: each copy names a file of its own, a new id and row (fileRows), and holds nothing else"},
}

// guardedUpdates are updates that name their file, each in a mutation that
// writes only the nodes its query finds: by is the function holding the
// mutation, and text its condition there.
var guardedUpdates = map[string]guardedUpdate{
	"business/Attachment/attachmentBusiness.go::ArchiveAttachmentByAttachmentUUID": {
		by:   "models/dgraph/Attachment/attachmentModel.go::UpdateDgraphAttachment",
		text: `Cond: "@if(gt(len(attachment), 0))"`,
		why:  "stamps the nodes of a file a task or a project lets go, and writes nothing when its query finds none",
	},
}

type guardedUpdate struct{ by, text, why string }

// notNodes make a DgraphAttachment that is never written as a node.
var notNodes = map[string]string{
	"business/Thread/docResource.go::clientFields": "the files a doc comment shows, sent as JSON with its thread",
}

// newAttachmentTimes are callers of NewAttachments whose time isn't only the
// write's, and why no request sets it.
var newAttachmentTimes = map[string]struct{ from, why string }{
	"business/Task/taskBusiness.go::CreateTaskComment": {"*createTaskCommentInfoInput.CreatedAt | time.Now()",
		"a backfill's time, which business/GitHub's sync sets in Go: the field is json:\"-\" (checked above)"},
}

// backdaters are the directories that may call helpers.WithImportTimestamp,
// which makes helpers.CreateTimeOrNow answer an earlier time.
var backdaters = map[string]string{
	"business/SlackImport": "the Slack import's message stages: an imported message's own time (they write no files; the file stage does)",
	"business/DemoSeed":    "the demo hub's seed, its messages spread over past days",
}

// The guard itself, on a small source: each kind of node is found, each rule
// holds, a time is followed through a parameter (and a func var) to its
// callers, and the tables answer for what they name.
func TestAttachmentNodesFindsEachKindAndRule(t *testing.T) {
	src := `package p

import (
	"time"

	"x/helpers"
	dg "x/models/dgraph"
)

func fresh() *dg.DgraphAttachment {
	now := time.Now()
	return &dg.DgraphAttachment{Uuid: "f", CreatedAt: &now}
}

func stamp(at time.Time) { _ = dg.DgraphAttachment{Uid: "uid(live)", DeletedAt: &at} }

func namedUpdate() { _ = dg.DgraphAttachment{Uid: "uid(f)", Uuid: "f"} }

func untimed() { _ = &dg.DgraphAttachment{Uuid: "f"} }

func elided() { _ = []*dg.DgraphAttachment{{Uuid: "f"}} }

func made() {
	a := new(dg.DgraphAttachment)
	a.Uuid = "f"
}

func declared() {
	var a dg.DgraphAttachment
	a.Uuid = "f"
}

func copied(fs []*dg.DgraphAttachment, at time.Time) {
	for _, f := range fs {
		c := *f
		c.Uid = ""
		c.CreatedAt = &at
	}
}

func copiedUntimed(f *dg.DgraphAttachment) {
	c := *f
	c.Uid = ""
}

func later() {
	a := &dg.DgraphAttachment{Uuid: "f"}
	now := time.Now().UTC()
	a.CreatedAt = &now
}

func wrongTime() {
	then := time.Unix(0, 0)
	_ = &dg.DgraphAttachment{Uuid: "f", CreatedAt: &then}
}

func notANode() { _ = dg.DgraphAttachment{FileName: "f"} }

func calls(in []*dg.DgraphAttachment, at time.Time) {
	now := time.Now()
	_ = dg.NewAttachments(in, "", now)
	_ = dg.NewAttachments(in, "", at)
}

func viaParam(in []*dg.DgraphAttachment, at time.Time) { _ = dg.NewAttachments(in, "", at) }

var write = viaVar

func viaVar(in []*dg.DgraphAttachment, at time.Time) { _ = dg.NewAttachments(in, "", at) }

func viaBadParam(in []*dg.DgraphAttachment, at time.Time) { _ = dg.NewAttachments(in, "", at) }

func callers(ctx context.Context, in []*dg.DgraphAttachment) {
	now := helpers.CreateTimeOrNow(ctx)
	viaParam(in, now)
	write(in, time.Now())
	viaBadParam(in, time.Unix(0, 0))
}

func backdated(ctx context.Context) context.Context { return helpers.WithImportTimestamp(ctx, time.Unix(0, 0)) }
`
	s := scanAttachmentNodes(t, map[string][]byte{"p/p.go": []byte(src)})
	if len(s.nodes) != 12 || len(s.calls) != 5 || len(s.backdates) != 1 {
		t.Fatalf("found %d nodes, %d calls and %d backdates, want 12, 5 and 1", len(s.nodes), len(s.calls), len(s.backdates))
	}
	newTimes := map[string]struct{ from, why string }{
		"p/p.go::fresh":     {"time.Now()", ""},
		"p/p.go::copied":    {"param:at", ""},
		"p/p.go::later":     {"time.Now().UTC()", ""},
		"p/p.go::wrongTime": {"time.Now()", ""},
		"p/p.go::gone":      {"time.Now()", ""},
	}
	problems := s.problems(newTimes, nil, map[string]string{"p/p.go::notANode": ""}, nil, map[string]string{})
	var got []string
	for _, p := range problems {
		rest := p[strings.Index(p, "p/p.go::")+len("p/p.go::"):]
		got = append(got, rest[:strings.IndexAny(rest, " :")])
	}
	sort.Strings(got)
	want := []string{"backdated", "calls", "copiedUntimed", "declared", "elided", "gone", "made", "namedUpdate", "untimed", "viaBadParam", "wrongTime"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("flagged %v, want %v:\n%s", got, want, strings.Join(problems, "\n"))
	}
}

// An attachmentNode is a place a function makes an attachment node: how
// ("literal", "elided", "copy", "new", "var"), and the fields it sets with
// their values (a named one's include those set on its name after; a copy
// names its source's file).
type attachmentNode struct {
	at, in, how string
	fields      map[string]ast.Expr
	fn          *ast.FuncDecl
	file        *goFile
}

// A timedCall is a call of NewAttachments and the time it passes.
type timedCall struct {
	at, in string
	time   ast.Expr
	fn     *ast.FuncDecl
	file   *goFile
}

// A goFile is a parsed file; a goPkg the files of one directory, its
// functions by name (a method by its own), and the package vars holding one.
type goFile struct {
	rel string
	f   *ast.File
	pkg *goPkg
}

type goPkg struct {
	files []*goFile
	funcs map[string][]*ast.FuncDecl
	alias map[string]string
}

type attachmentScan struct {
	fset      *token.FileSet
	nodes     []attachmentNode
	calls     []timedCall
	backdates []string          // "rel::func" of each call of helpers.WithImportTimestamp
	source    map[string]string // each function's text, by "rel::func"
	followed  map[string]bool
}

// scanAttachmentNodes is every attachment node and call of NewAttachments in
// files (by path from the module's root).
func scanAttachmentNodes(t *testing.T, files map[string][]byte) *attachmentScan {
	t.Helper()
	s := &attachmentScan{fset: token.NewFileSet(), source: map[string]string{}, followed: map[string]bool{}}
	pkgs := map[string]*goPkg{}
	var all []*goFile
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		f, err := parser.ParseFile(s.fset, rel, files[rel], parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		dir := path.Dir(rel)
		p := pkgs[dir]
		if p == nil {
			p = &goPkg{funcs: map[string][]*ast.FuncDecl{}, alias: map[string]string{}}
			pkgs[dir] = p
		}
		gf := &goFile{rel: rel, f: f, pkg: p}
		p.files = append(p.files, gf)
		all = append(all, gf)
		for _, d := range f.Decls {
			switch x := d.(type) {
			case *ast.FuncDecl:
				p.funcs[x.Name.Name] = append(p.funcs[x.Name.Name], x)
				from, to := s.fset.Position(x.Pos()).Offset, s.fset.Position(x.End()).Offset
				s.source[rel+"::"+declName(x)] = string(files[rel][from:to])
			case *ast.GenDecl:
				for _, spec := range x.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for i, n := range vs.Names {
							if i < len(vs.Values) {
								if id, ok := vs.Values[i].(*ast.Ident); ok {
									p.alias[n.Name] = id.Name
								}
							}
						}
					}
				}
			}
		}
	}
	for _, gf := range all {
		s.scanFile(gf)
	}
	return s
}

func (s *attachmentScan) scanFile(gf *goFile) {
	f, rel := gf.f, gf.rel
	inPkg := path.Dir(rel) == "models/dgraph"
	aliases := map[string]bool{}
	helpersName := ""
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name := p[strings.LastIndex(p, "/")+1:]
		if strings.HasSuffix(p, "/models/dgraph") {
			name = "models"
		}
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch {
		case strings.HasSuffix(p, "/models/dgraph"):
			aliases[name] = true
		case strings.HasSuffix(p, "/helpers"):
			helpersName = name
		}
	}
	isAtt := func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.Ident:
			return inPkg && x.Name == "DgraphAttachment"
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			return ok && aliases[id.Name] && x.Sel.Name == "DgraphAttachment"
		}
		return false
	}
	isPtr := func(e ast.Expr) bool {
		st, ok := e.(*ast.StarExpr)
		return ok && isAtt(st.X)
	}
	isList := func(e ast.Expr) bool {
		var el ast.Expr
		switch x := e.(type) {
		case *ast.ArrayType:
			el = x.Elt
		case *ast.MapType:
			el = x.Value
		}
		return el != nil && (isAtt(el) || isPtr(el))
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		in := rel + "::" + declName(fd)
		pos := func(n ast.Node) string { return s.fset.Position(n.Pos()).String() }
		// Names of pointers to a node and of lists of them, from the
		// function's parameters and vars; a range over a list gives pointers.
		ptrs, lists := map[string]bool{}, map[string]bool{}
		name := func(names []*ast.Ident, typ ast.Expr) {
			for _, n := range names {
				if isPtr(typ) {
					ptrs[n.Name] = true
				} else if isList(typ) {
					lists[n.Name] = true
				}
			}
		}
		for _, fl := range fd.Type.Params.List {
			name(fl.Names, fl.Type)
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				if x.Type != nil {
					name(x.Names, x.Type)
				}
			case *ast.RangeStmt:
				if id, ok := x.X.(*ast.Ident); ok && lists[id.Name] {
					if v, ok := x.Value.(*ast.Ident); ok {
						ptrs[v.Name] = true
					}
				}
			}
			return true
		})
		// A node assigned to a name: the fields set on that name later are
		// its fields too.
		named := map[ast.Node]string{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == len(as.Rhs) {
				for i, r := range as.Rhs {
					if u, ok := r.(*ast.UnaryExpr); ok && u.Op == token.AND {
						r = u.X
					}
					if id, ok := as.Lhs[i].(*ast.Ident); ok {
						named[r] = id.Name
					}
				}
			}
			return true
		})
		node := func(n ast.Node, how, varName string, fields map[string]ast.Expr) {
			if fields == nil {
				fields = map[string]ast.Expr{}
			}
			if varName != "" {
				for k, v := range fieldsSetOn(fd.Body, varName) {
					fields[k] = v
				}
			}
			s.nodes = append(s.nodes, attachmentNode{at: pos(n), in: in, how: how, fields: fields, fn: fd, file: gf})
		}
		keys := func(cl *ast.CompositeLit) map[string]ast.Expr {
			out := map[string]ast.Expr{}
			for _, el := range cl.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok {
						out[k.Name] = kv.Value
					}
				}
			}
			return out
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if isAtt(x.Type) {
					node(x, "literal", named[x], keys(x))
				} else if x.Type != nil && isList(x.Type) {
					for _, el := range x.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							el = kv.Value
						}
						if cl, ok := el.(*ast.CompositeLit); ok && cl.Type == nil {
							node(cl, "elided", "", keys(cl))
						}
					}
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 && isAtt(x.Args[0]) {
					node(x, "new", named[x], nil)
				}
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok {
						switch {
						case aliases[id.Name] && sel.Sel.Name == "NewAttachments" && len(x.Args) == 3:
							s.calls = append(s.calls, timedCall{at: pos(x), in: in, time: x.Args[2], fn: fd, file: gf})
						case helpersName != "" && id.Name == helpersName && sel.Sel.Name == "WithImportTimestamp":
							s.backdates = append(s.backdates, in)
						}
					}
				}
			case *ast.ValueSpec:
				if x.Type != nil && isAtt(x.Type) {
					for _, n := range x.Names {
						node(x, "var", n.Name, nil)
					}
				}
			case *ast.AssignStmt:
				if len(x.Lhs) != len(x.Rhs) {
					break
				}
				for i, r := range x.Rhs {
					st, ok := r.(*ast.StarExpr)
					if !ok {
						continue
					}
					src, ok := st.X.(*ast.Ident)
					dst, ok2 := x.Lhs[i].(*ast.Ident)
					if ok && ok2 && ptrs[src.Name] {
						// A copy names its source's file.
						node(x, "copy", dst.Name, map[string]ast.Expr{"Uuid": st})
					}
				}
			}
			return true
		})
	}
}

// problems is why each node, call or backdate breaks the rules above, and
// each table entry no longer found.
func (s *attachmentScan) problems(newTimes map[string]struct{ from, why string }, guarded map[string]guardedUpdate, notNode map[string]string,
	callTimes map[string]struct{ from, why string }, backdate map[string]string) (out []string) {
	seen := map[string]bool{}
	for _, n := range s.nodes {
		where := n.at + " in " + n.in
		if _, ok := notNode[n.in]; ok {
			seen[n.in] = true
			continue
		}
		if uid, ok := n.fields["Uid"]; ok && !isEmptyString(uid) {
			if _, names := n.fields["Uuid"]; !names {
				continue
			}
			if g, ok := guarded[n.in]; ok {
				seen[n.in] = true
				if !strings.Contains(s.source[g.by], g.text) {
					out = append(out, where+": its mutation in "+g.by+" no longer holds "+g.text+", so a query finding nothing makes a node naming the file with no time")
				}
				continue
			}
			out = append(out, where+": an update naming its file, in a mutation that may find nothing, makes a node naming the file with no time; don't set Uuid, or guard the mutation and add it to guardedUpdates")
			continue
		}
		at, timed := n.fields["CreatedAt"]
		if !timed {
			out = append(out, where+": makes a "+n.how+" attachment node with no time (CreatedAt); a node with no time holds every file it names (business/Attachment ArchiveFilesOf)")
			continue
		}
		got := s.timeOf(n.file, n.fn, at)
		want, ok := newTimes[n.in]
		switch {
		case !ok:
			out = append(out, where+": a new writer of attachment nodes, at "+got+"; add it to newNodeTimes with where its time comes from")
		case got != want.from:
			seen[n.in] = true
			out = append(out, where+": writes its nodes at "+got+", not "+want.from)
		default:
			seen[n.in] = true
		}
	}
	for _, c := range s.calls {
		got := s.timeOf(c.file, c.fn, c.time)
		if want, ok := callTimes[c.in]; ok && got == want.from {
			seen[c.in] = true
			continue
		}
		for _, src := range strings.Split(got, " | ") {
			if !strings.HasPrefix(src, "time.Now()") && !strings.HasPrefix(src, "helpers.CreateTimeOrNow(") {
				out = append(out, c.at+" in "+c.in+": NewAttachments at "+got+", not the time of the write; a request mustn't choose its files' time")
				break
			}
		}
	}
	for _, in := range s.backdates {
		if _, ok := backdate[path.Dir(in[:strings.Index(in, "::")])]; !ok {
			out = append(out, in+": calls helpers.WithImportTimestamp, so helpers.CreateTimeOrNow answers it a time that isn't its write's; only backdaters may")
		}
	}
	guardedKeys := map[string]bool{}
	for k := range guarded {
		guardedKeys[k] = true
	}
	for _, table := range []map[string]bool{keysOf(newTimes), guardedKeys, keysOfStrings(notNode), keysOf(callTimes)} {
		for k := range table {
			if !seen[k] {
				out = append(out, k+": named in a table of attachmentNodeTimes_test.go but no longer found; take it out")
			}
		}
	}
	return out
}

// timeOf is every source of the time e in fn: for a name, every value
// assigned to it there; a parameter is followed to the calls of fn in its
// package (by name, or through a package var holding it), and is
// "param:<name>" when none passes it. Sorted, joined by " | ".
func (s *attachmentScan) timeOf(gf *goFile, fn *ast.FuncDecl, e ast.Expr) string {
	srcs := map[string]bool{}
	s.sources(gf, fn, e, 0, srcs)
	out := make([]string, 0, len(srcs))
	for src := range srcs {
		out = append(out, src)
	}
	sort.Strings(out)
	return strings.Join(out, " | ")
}

func (s *attachmentScan) sources(gf *goFile, fn *ast.FuncDecl, e ast.Expr, depth int, out map[string]bool) {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	id, ok := e.(*ast.Ident)
	if !ok {
		out[exprText(s.fset, e)] = true
		return
	}
	// A name followed once per function: a = b, b = a ends.
	key := s.fset.Position(fn.Pos()).String() + " " + id.Name
	if s.followed[key] {
		return
	}
	s.followed[key] = true
	defer delete(s.followed, key)
	assigned := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) == len(x.Rhs) {
				for i, l := range x.Lhs {
					if lid, ok := l.(*ast.Ident); ok && lid.Name == id.Name {
						assigned = true
						s.sources(gf, fn, x.Rhs[i], depth, out)
					}
				}
			}
		case *ast.ValueSpec:
			for i, n := range x.Names {
				if n.Name == id.Name && i < len(x.Values) {
					assigned = true
					s.sources(gf, fn, x.Values[i], depth, out)
				}
			}
		}
		return true
	})
	if i := paramIndex(fn, id.Name); i >= 0 {
		s.fromCallers(gf, fn, id.Name, i, depth, out)
	} else if !assigned {
		out[id.Name] = true
	}
}

func (s *attachmentScan) fromCallers(gf *goFile, fn *ast.FuncDecl, param string, i, depth int, out map[string]bool) {
	names := map[string]bool{fn.Name.Name: true}
	for v, f := range gf.pkg.alias {
		if f == fn.Name.Name {
			names[v] = true
		}
	}
	found := false
	if depth < 4 {
		for _, cf := range gf.pkg.files {
			for _, d := range cf.f.Decls {
				caller, ok := d.(*ast.FuncDecl)
				if !ok || caller.Body == nil {
					continue
				}
				ast.Inspect(caller.Body, func(n ast.Node) bool {
					ce, ok := n.(*ast.CallExpr)
					if !ok || len(ce.Args) <= i {
						return true
					}
					callee := ""
					switch x := ce.Fun.(type) {
					case *ast.Ident:
						callee = x.Name
					case *ast.SelectorExpr:
						if fn.Recv != nil {
							callee = x.Sel.Name
						}
					}
					if names[callee] {
						found = true
						s.sources(cf, caller, ce.Args[i], depth+1, out)
					}
					return true
				})
			}
		}
	}
	if !found {
		out["param:"+param] = true
	}
}

func paramIndex(fn *ast.FuncDecl, name string) int {
	i := 0
	for _, fl := range fn.Type.Params.List {
		if len(fl.Names) == 0 {
			i++
			continue
		}
		for _, n := range fl.Names {
			if n.Name == name {
				return i
			}
			i++
		}
	}
	return -1
}

// fieldsSetOn is the fields assigned on the name v in body, with their values.
func fieldsSetOn(body *ast.BlockStmt, v string) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, l := range as.Lhs {
			if sel, ok := l.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == v {
					out[sel.Sel.Name] = as.Rhs[i]
				}
			}
		}
		return true
	})
	return out
}

// fieldTag is the tag of field in the struct type named typ in src.
func fieldTag(t *testing.T, src []byte, typ, field string) string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "src.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	tag := ""
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != typ {
			return true
		}
		if st, ok := ts.Type.(*ast.StructType); ok {
			for _, fl := range st.Fields.List {
				for _, n := range fl.Names {
					if n.Name == field && fl.Tag != nil {
						tag, _ = strconv.Unquote(fl.Tag.Value)
					}
				}
			}
		}
		return false
	})
	if tag == "" {
		t.Fatalf("no tagged field %s in %s", field, typ)
	}
	return tag
}

func isEmptyString(e ast.Expr) bool {
	b, ok := e.(*ast.BasicLit)
	return ok && b.Kind == token.STRING && (b.Value == `""` || b.Value == "``")
}

// declName is a function's name, a method's with its receiver's type.
func declName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	typ := fd.Recv.List[0].Type
	if st, ok := typ.(*ast.StarExpr); ok {
		typ = st.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

func exprText(fset *token.FileSet, e ast.Expr) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, e); err != nil {
		return "?"
	}
	return b.String()
}

func keysOf(m map[string]struct{ from, why string }) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

func keysOfStrings(m map[string]string) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}
