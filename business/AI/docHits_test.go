package business

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// A search hit on a doc, or on a comment on one, stays only if the person can
// read the doc now; everything else passes untouched, and each doc is read once.
func TestKeepReadableDocs(t *testing.T) {
	yes, no := true, false
	docs := map[string]*dgraphStruct.DgraphDoc{
		"open":    {Uuid: "open", IsPrivate: &no},
		"secret":  {Uuid: "secret", IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "owner"}},
		"shared":  {Uuid: "shared", IsPrivate: &yes, HasReadAccess: 1, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "owner"}},
		"theirs":  {Uuid: "theirs", IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "me-uuid"}},
		"missing": nil,
	}
	reads := map[string]int{}
	restore := readDocFor
	defer func() { readDocFor = restore }()
	readDocFor = func(_ context.Context, docUUID, userUID string) (*dgraphStruct.DgraphDoc, error) {
		reads[docUUID]++
		if userUID != "0xme" {
			t.Errorf("read %s as %q", docUUID, userUID)
		}
		if docUUID == "broken" {
			return nil, errors.New("dgraph is down")
		}
		return docs[docUUID], nil
	}
	me := &userModels.UserInfo{}
	me.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: "0xme", Uuid: "me-uuid"}

	hits := []ai.SimilarResult{
		{ContentType: "post", ContentUUID: "p1", ChannelUUID: "ch"},
		{ContentType: "doc", ContentUUID: "open"},
		{ContentType: "doc", ContentUUID: "secret"},
		{ContentType: "comment", ContentUUID: "c1", DocUUID: "secret"},
		{ContentType: "comment", ContentUUID: "c2", DocUUID: "shared"},
		{ContentType: "doc", ContentUUID: "shared"},
		{ContentType: "doc", ContentUUID: "theirs"},
		{ContentType: "doc", ContentUUID: "missing"},
		{ContentType: "doc", ContentUUID: "broken"},
		{ContentType: "comment", ContentUUID: "c3", PostUUID: "p1", ChannelUUID: "ch"},
		{ContentType: "chat", ContentUUID: "m1"},
	}
	var kept []string
	for _, h := range keepReadableDocs(context.Background(), me, hits) {
		kept = append(kept, h.ContentUUID)
	}
	if got, want := strings.Join(kept, ","), "p1,open,c2,shared,theirs,c3,m1"; got != want {
		t.Errorf("kept %s, want %s", got, want)
	}
	for doc, n := range reads {
		if n != 1 {
			t.Errorf("%s was read %d times", doc, n)
		}
	}
	if len(reads) != 6 {
		t.Errorf("read %d docs, want the 6 named: %v", len(reads), reads)
	}
	if got := keepReadableDocs(context.Background(), me, nil); len(got) != 0 {
		t.Errorf("no hits: %v", got)
	}
}

// The searches across the workspace can return docs, so every call of them
// goes through docHits.go, which drops the ones the person can't read. A
// search called directly would hand back what the index says, and the index
// said a private doc was public for as long as a save had rewritten it.
func TestWorkspaceSearchesGoThroughTheDocCheck(t *testing.T) {
	root := filepath.Join("..", "..")
	guarded := map[string]bool{"SearchSimilar": true, "SearchRecentGlobal": true}
	var offenders []string
	scanned := 0
	for _, dir := range []string{"adapter", "business", "cmd", "controllers", "domain", "helpers", "middleware", "router", "services"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			// Where they're defined, and the one place that calls them.
			if strings.HasPrefix(rel, "services/AI/") || rel == "business/AI/docHits.go" {
				return nil
			}
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if perr != nil {
				return perr
			}
			scanned++
			ast.Inspect(f, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && guarded[sel.Sel.Name] {
						offenders = append(offenders, rel+": "+sel.Sel.Name)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 500 {
		t.Fatalf("scanned %d files: the walk no longer reaches the source", scanned)
	}
	if len(offenders) > 0 {
		t.Fatalf("searched the workspace without the doc check; call searchSimilar or searchRecentGlobal "+
			"(business/AI/docHits.go) instead:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// A doc hit in a search an agent makes for someone other than its sponsor is
// kept only if the doc, read for each of them, says both may read it. The
// index's copy of who may read a doc can lag (it is written after the doc), so
// it cannot be the only thing standing between the asker and the sponsor's
// private docs.
func TestADocHitForSomeoneElseIsReadableByThemToo(t *testing.T) {
	yes, no := true, false
	// Each doc as read for the sponsor (0xsana) and for the asker (0xravi):
	// sharing counts belong to the reader a doc is read for.
	docs := map[string]map[string]*dgraphStruct.DgraphDoc{
		"open":   {"0xsana": {Uuid: "open", IsPrivate: &no}, "0xravi": {Uuid: "open", IsPrivate: &no}},
		"hers":   {"0xsana": {Uuid: "hers", IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "sana"}}, "0xravi": {Uuid: "hers", IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "sana"}}},
		"shared": {"0xsana": {Uuid: "shared", IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "sana"}}, "0xravi": {Uuid: "shared", IsPrivate: &yes, HasReadAccess: 1, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "sana"}}},
	}
	restoreRead, restorePerson := readDocFor, lookupPerson
	t.Cleanup(func() { readDocFor, lookupPerson = restoreRead, restorePerson })
	readDocFor = func(_ context.Context, docUUID, userUID string) (*dgraphStruct.DgraphDoc, error) {
		return docs[docUUID][userUID], nil
	}
	people := map[string]*dgraphStruct.DgraphUser{"ravi": {Uid: "0xravi", Uuid: "ravi"}}
	lookupPerson = func(_ context.Context, uuid string) (*dgraphStruct.DgraphUser, error) {
		if p, ok := people[uuid]; ok {
			return p, nil
		}
		return nil, errors.New("no such person")
	}
	sponsor := &userModels.UserInfo{}
	sponsor.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: "0xsana", Uuid: "sana"}
	hits := []ai.SimilarResult{
		{ContentType: "post", ContentUUID: "p1", ChannelUUID: "ch"},
		{ContentType: "doc", ContentUUID: "open"},
		{ContentType: "doc", ContentUUID: "hers"},
		{ContentType: "comment", ContentUUID: "c1", DocUUID: "hers"},
		{ContentType: "doc", ContentUUID: "shared"},
	}
	kept := func(ctx context.Context) string {
		var ids []string
		for _, h := range keepReadableDocs(ctx, sponsor, hits) {
			ids = append(ids, h.ContentUUID)
		}
		return strings.Join(ids, ",")
	}
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"the sponsor's own search", ai.WithoutRunRequester(context.Background()), "p1,open,hers,c1,shared"},
		{"for someone else", ai.WithRunRequester(context.Background(), "ravi", "sana"), "p1,open,shared"},
		{"for someone unidentified", ai.WithRunRequester(context.Background(), "", "sana"), "p1"},
		{"for someone who can't be resolved", ai.WithRunRequester(context.Background(), "gone", "sana"), "p1"},
	}
	for _, c := range cases {
		if got := kept(c.ctx); got != c.want {
			t.Errorf("%s: kept %s, want %s", c.name, got, c.want)
		}
	}
}
