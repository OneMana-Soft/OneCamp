package helpers

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every graph uid a request can carry is accounted for.
//
// WHY. A uid from a client names any node: uids are sequential and many
// answers carry them. Written into a mutation unchecked, it lets a member
// change or delete a node that isn't theirs: reactions did it until 12 Oct
// 2026 (reactionBusiness.MayChange), and every write that adds files did it
// until 19 Oct (dgraphStruct.NewAttachments). This finds each request field
// in adapter/ and controllers/ that can carry one: a json name of uid, *_uid
// or *_dgraph_id, a Go name ending in Uid or DgraphID, or a graph struct
// (models/dgraph), which carries a uid of its own. Each must be listed in
// testdata/uid-trust.txt with how it is made safe: checked MayChange-style
// (node type, parent, owner) before any write, replaced from a node the
// server read, or never read from a request. The list's reasons name the
// guard or test that holds each one. A field missing from the list, or a
// line naming a field that no longer exists, fails.
func TestEveryUidARequestCarriesIsAccountedFor(t *testing.T) {
	found := map[string]bool{}
	for _, root := range []string{"../adapter", "../controllers"} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, f := range uidCarryingFields(t, strings.TrimPrefix(filepath.ToSlash(path), "../"), src) {
				found[f] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(found) < 20 {
		t.Fatalf("found %d request fields that carry a uid; the scan has stopped seeing them", len(found))
	}
	listed := readUidTrust(t, "testdata/uid-trust.txt")
	var missing, stale []string
	for f := range found {
		if _, ok := listed[f]; !ok {
			missing = append(missing, f)
		}
	}
	for f := range listed {
		if !found[f] {
			stale = append(stale, f)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	for _, f := range missing {
		t.Errorf("%s can carry a graph uid from a client. Check it MayChange-style (the node's type, parent and owner, in the transaction that writes) or replace it from a node the server read, before any write; then list it in helpers/testdata/uid-trust.txt with how, and the test that holds it", f)
	}
	for _, f := range stale {
		t.Errorf("helpers/testdata/uid-trust.txt lists %s, which no longer exists: take its line out", f)
	}
}

// The guard itself.
func TestUidCarryingFieldsFindsThem(t *testing.T) {
	src := `package p

import (
	models "x/models/dgraph"
)

type In struct {
	ReactionDgraphUid string ` + "`json:\"reaction_dgraph_id\"`" + `
	ChannelUUID       string ` + "`json:\"channel_uuid\"`" + `
	Node              string ` + "`json:\"uid\"`" + `
	ParentUID         string
	Files             []*models.DgraphAttachment ` + "`json:\"files\"`" + `
	Name              string ` + "`json:\"name\"`" + `
	*models.DgraphProject
}

func h() {
	var body struct {
		TargetUid string ` + "`json:\"target_uid\"`" + `
		DocUUID   string ` + "`json:\"doc_uuid\"`" + `
	}
	_ = body
}
`
	got := uidCarryingFields(t, "p.go", []byte(src))
	want := "p.go :: In.DgraphProject,p.go :: In.Files,p.go :: In.Node,p.go :: In.ParentUID,p.go :: In.ReactionDgraphUid,p.go :: h.body.TargetUid"
	if strings.Join(got, ",") != want {
		t.Errorf("found\n  %s\nwant\n  %s", strings.Join(got, ","), want)
	}
}

var (
	uidJSON = regexp.MustCompile(`^(uid|.*_uid|.*_uids|.*_dgraph_id|.*_dgraph_ids)$`)
	uidName = regexp.MustCompile(`(Uid|UID|DgraphID|DgraphId)s?$`)
)

// uidCarryingFields is each field in src's struct types (named, or a var's
// anonymous struct) that can carry a graph uid, as "path :: Type.Field"
// ("path :: Func.var.Field" for a var's), sorted.
func uidCarryingFields(t *testing.T, path string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	graphPkgs := map[string]bool{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if !strings.HasSuffix(p, "/models/dgraph") {
			continue
		}
		name := "dgraph"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		graphPkgs[name] = true
	}
	isGraph := func(e ast.Expr) (string, bool) {
		for {
			switch x := e.(type) {
			case *ast.StarExpr:
				e = x.X
				continue
			case *ast.ArrayType:
				e = x.Elt
				continue
			case *ast.MapType:
				e = x.Value
				continue
			case *ast.SelectorExpr:
				pkg, ok := x.X.(*ast.Ident)
				return x.Sel.Name, ok && graphPkgs[pkg.Name]
			}
			return "", false
		}
	}
	var out []string
	fields := func(owner string, st *ast.StructType) {
		for _, fl := range st.Fields.List {
			tag := ""
			if fl.Tag != nil {
				raw, _ := strconv.Unquote(fl.Tag.Value)
				tag = strings.Split(reflect.StructTag(raw).Get("json"), ",")[0]
			}
			typeName, graph := isGraph(fl.Type)
			if len(fl.Names) == 0 {
				if graph {
					out = append(out, path+" :: "+owner+"."+typeName)
				}
				continue
			}
			for _, n := range fl.Names {
				byName := uidName.MatchString(n.Name) && !strings.HasSuffix(strings.ToUpper(n.Name), "UUID") && !strings.HasSuffix(strings.ToUpper(n.Name), "UUIDS")
				if graph || uidJSON.MatchString(tag) || byName {
					out = append(out, path+" :: "+owner+"."+n.Name)
				}
			}
		}
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if st, ok := s.Type.(*ast.StructType); ok {
						fields(s.Name.Name, st)
					}
				case *ast.ValueSpec:
					if st, ok := s.Type.(*ast.StructType); ok {
						for _, n := range s.Names {
							fields(n.Name, st)
						}
					}
				}
			}
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			ast.Inspect(d.Body, func(n ast.Node) bool {
				switch s := n.(type) {
				case *ast.TypeSpec:
					if st, ok := s.Type.(*ast.StructType); ok {
						fields(d.Name.Name+"."+s.Name.Name, st)
					}
				case *ast.ValueSpec:
					if st, ok := s.Type.(*ast.StructType); ok {
						for _, v := range s.Names {
							fields(d.Name.Name+"."+v.Name, st)
						}
					}
				}
				return true
			})
		}
	}
	sort.Strings(out)
	return out
}

// readUidTrust is the list's fields and their reasons: "path :: Type.Field
// :: reason" per line, # for comments. A line without a reason fails.
func readUidTrust(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, " :: ", 3)
		if len(parts) != 3 || strings.TrimSpace(parts[2]) == "" {
			t.Errorf("%s: %q needs a path, a field and a reason, joined by \" :: \"", path, line)
			continue
		}
		out[parts[0]+" :: "+parts[1]] = parts[2]
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
