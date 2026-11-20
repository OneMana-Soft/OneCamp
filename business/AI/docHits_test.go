package business

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// A search hit on a doc, or on a comment on one, stays only if the person can
// read the doc now; everything else passes untouched, and the docs are read in
// one call, each named once.
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
	calls := 0
	restore := readableDocs
	defer func() { readableDocs = restore }()
	readableDocs = readDocsAs(func(docUUID, userUUID string) *dgraphStruct.DgraphDoc {
		reads[docUUID]++
		if userUUID != "me-uuid" {
			t.Errorf("read %s as %q", docUUID, userUUID)
		}
		return docs[docUUID]
	}, &calls)
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
	if len(reads) != 5 || calls != 1 {
		t.Errorf("read %d docs in %d calls, want the 5 named in 1: %v", len(reads), calls, reads)
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
	guarded := map[string]bool{"SearchSimilar": true, "SearchRecentGlobal": true, "FetchUnreadAcrossScopes": true}
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
		t.Fatalf("searched the workspace without the doc check; call searchSimilar, searchRecentGlobal or "+
			"fetchUnreadAcrossScopes (business/AI/docHits.go) instead:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// A doc hit in a search an agent makes for someone other than its sponsor is
// kept only if the doc, read for each of them, says both may read it. The
// index's copy of who may read a doc can lag (it is written after the doc), so
// it cannot be the only thing standing between the asker and the sponsor's
// private docs.
func TestADocHitForSomeoneElseIsReadableByThemToo(t *testing.T) {
	yes, no := true, false
	// Each doc as read for the sponsor (sana) and for the asker (ravi):
	// sharing counts belong to the reader a doc is read for.
	docs := map[string]map[string]*dgraphStruct.DgraphDoc{
		"open":   {"sana": {Uuid: "open", IsPrivate: &no}, "ravi": {Uuid: "open", IsPrivate: &no}},
		"hers":   {"sana": {Uuid: "hers", IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "sana"}}, "ravi": {Uuid: "hers", IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "sana"}}},
		"shared": {"sana": {Uuid: "shared", IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "sana"}}, "ravi": {Uuid: "shared", IsPrivate: &yes, HasReadAccess: 1, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "sana"}}},
	}
	restoreRead, restorePerson := readableDocs, lookupPerson
	t.Cleanup(func() { readableDocs, lookupPerson = restoreRead, restorePerson })
	readableDocs = readDocsAs(func(docUUID, userUUID string) *dgraphStruct.DgraphDoc {
		return docs[docUUID][userUUID]
	}, nil)
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

// readDocsAs stands in for the graph behind docBusiness.ReadableDocsFor: each
// doc as read for a reader (by uuid; nil for none), held to the doc rule, its
// deletion included. calls, when given, counts the asks.
func readDocsAs(doc func(docUUID, userUUID string) *dgraphStruct.DgraphDoc, calls *int) func(context.Context, []string, string) (map[string]bool, error) {
	return func(_ context.Context, ids []string, userUUID string) (map[string]bool, error) {
		if calls != nil {
			*calls++
		}
		out := map[string]bool{}
		for _, id := range ids {
			if d := doc(id, userUUID); d != nil {
				out[strings.ToLower(id)] = docBusiness.CanRead(d, userUUID)
			}
		}
		return out, nil
	}
}

// readBoardsAs stands in for the graph behind boardBusiness.ReadableBoardsFor:
// boards, read for the person with their deletion, held to the board rule.
func readBoardsAs(boards map[string]*dgraphStruct.DgraphBoard, reads map[string]int) func(context.Context, []string, string) (map[string]bool, error) {
	return func(_ context.Context, ids []string, userUUID string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, id := range ids {
			reads[id]++
			if id == "broken" {
				return nil, errors.New("dgraph is down")
			}
			if b, ok := boards[id]; ok {
				out[strings.ToLower(id)] = boardBusiness.CanRead(b, userUUID)
			}
		}
		return out, nil
	}
}

// A comment on a board stays only if the person can read the board now: the
// entry's copy of the board's privacy is from when the comment was written,
// and the board's deletion may not have reached it.
func TestKeepReadableBoardComments(t *testing.T) {
	yes, no := true, false
	deleted := time.Now().Add(-time.Hour)
	boards := map[string]*dgraphStruct.DgraphBoard{
		"open":    {Uuid: "open", IsPrivate: &no},
		"private": {Uuid: "private", IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "owner"}},
		"shared":  {Uuid: "shared", IsPrivate: &yes, HasCommentAccess: 1, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "owner"}},
		"binned":  {Uuid: "binned", IsPrivate: &no, DeletedAt: &deleted},
	}
	reads := map[string]int{}
	restore := readableBoards
	defer func() { readableBoards = restore }()
	readableBoards = readBoardsAs(boards, reads)
	me := &userModels.UserInfo{}
	me.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: "0xme", Uuid: "me-uuid"}

	hits := []ai.SimilarResult{
		{ContentType: "comment", ContentUUID: "c-open", BoardUUID: "open"},
		{ContentType: "comment", ContentUUID: "c-private", BoardUUID: "private"},
		{ContentType: "comment", ContentUUID: "c-private-2", BoardUUID: "private"},
		{ContentType: "comment", ContentUUID: "c-shared", BoardUUID: "shared"},
		{ContentType: "comment", ContentUUID: "c-binned", BoardUUID: "binned"},
		{ContentType: "comment", ContentUUID: "c-gone", BoardUUID: "gone"},
		// A board's own entry (what it says) is the board's to grant.
		{ContentType: ai.BoardContentType, ContentUUID: "private", BoardUUID: "private"},
		{ContentType: ai.BoardContentType, ContentUUID: "shared", BoardUUID: "shared"},
		{ContentType: "post", ContentUUID: "p1", ChannelUUID: "ch"},
	}
	var kept []string
	for _, h := range keepReadableDocs(context.Background(), me, hits) {
		kept = append(kept, h.ContentUUID)
	}
	if got, want := strings.Join(kept, ","), "c-open,c-shared,shared,p1"; got != want {
		t.Errorf("kept %s, want %s", got, want)
	}
	if reads["private"] != 1 {
		t.Errorf("the private board was read %d times, want once", reads["private"])
	}
	// The boards are read in one call: when it fails, no board hit is kept.
	kept = kept[:0]
	broken := append(hits, ai.SimilarResult{ContentType: "comment", ContentUUID: "c-broken", BoardUUID: "broken"})
	for _, h := range keepReadableDocs(context.Background(), me, broken) {
		kept = append(kept, h.ContentUUID)
	}
	if got, want := strings.Join(kept, ","), "p1"; got != want {
		t.Errorf("the board read failed: kept %s, want %s", got, want)
	}
}

// A deleted board's comments stayed in AI search and catch-up whenever the
// delete's cascade missed their entries: the check read the board without its
// deletion time (GetBasicBoardByUUID), so a deleted board read as live. It now
// asks boardBusiness.ReadableBoardsFor, which reads it.
func TestTheBoardCheckReadsTheBoardsDeletion(t *testing.T) {
	if reflect.ValueOf(readableBoards).Pointer() != reflect.ValueOf(boardBusiness.ReadableBoardsFor).Pointer() {
		t.Fatal("AI search checks boards with something other than boardBusiness.ReadableBoardsFor, which reads a board's deletion")
	}
}

// The workspace catch-up window holds docs and comments on docs and boards
// too (only memory items are left out), so it is held to the same check: an
// item about a doc or a board the person can't read now is dropped.
func TestCatchUpItemsAreCheckedLikeSearchHits(t *testing.T) {
	yes := true
	restoreDoc, restoreBoard := readableDocs, readableBoards
	defer func() { readableDocs, readableBoards = restoreDoc, restoreBoard }()
	no := false
	deleted := time.Now().Add(-time.Hour)
	readableDocs = readDocsAs(func(docUUID, _ string) *dgraphStruct.DgraphDoc {
		if docUUID == "binned" {
			// Readable by everyone, but deleted.
			return &dgraphStruct.DgraphDoc{Uuid: docUUID, IsPrivate: &no, DeletedAt: &deleted}
		}
		return &dgraphStruct.DgraphDoc{Uuid: docUUID, IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "owner"}}
	}, nil)
	readableBoards = readBoardsAs(map[string]*dgraphStruct.DgraphBoard{
		"roadmap": {Uuid: "roadmap", IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "owner"}},
		// Readable by everyone, but deleted.
		"retro": {Uuid: "retro", IsPrivate: &no, DeletedAt: &deleted},
	}, map[string]int{})
	me := &userModels.UserInfo{}
	me.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: "0xme", Uuid: "me-uuid"}

	items := []ai.ScopedContent{
		{ContentText: "standup notes", ContentType: "post", ContentUUID: "p1"},
		{ContentText: "the old plan", ContentType: "doc", ContentUUID: "binned"},
		{ContentText: "the reorg plan", ContentType: "doc", ContentUUID: "plans"},
		{ContentText: "agreed", ContentType: "comment", ContentUUID: "c1", DocUUID: "plans"},
		{ContentText: "move this box", ContentType: "comment", ContentUUID: "c2", BoardUUID: "roadmap"},
		{ContentText: "keep the timer", ContentType: "comment", ContentUUID: "c3", BoardUUID: "retro"},
		{ContentText: "ship it", ContentType: "task", ContentUUID: "t1"},
	}
	var kept []string
	for _, it := range keepReadable(context.Background(), me, items, func(c ai.ScopedContent) readRef {
		return refOf(c.ContentType, c.ContentUUID, c.DocUUID, c.BoardUUID)
	}) {
		kept = append(kept, it.ContentText)
	}
	if got, want := strings.Join(kept, ","), "standup notes,ship it"; got != want {
		t.Errorf("kept %s, want %s", got, want)
	}
}

// Every doc hit is asked about in one call per reader, and every board in one
// more (A1-13): five doc hits and two boards make one doc call and one board
// call for the person searching, and two of each when a run acts for someone
// else. The answers are each doc's own: a private doc among open ones is
// still dropped, and a call that fails drops every hit of its kind.
func TestDocHitsAreAskedInOneCallPerReader(t *testing.T) {
	yes, no := true, false
	owner := &dgraphStruct.DgraphUser{Uuid: "owner"}
	// Each doc as read for a reader (by uuid): sharing counts are the reader's.
	docs := map[string]map[string]*dgraphStruct.DgraphDoc{
		"sana": {
			"open":   {Uuid: "open", IsPrivate: &no},
			"secret": {Uuid: "secret", IsPrivate: &yes, CreatedBy: owner},
			"shared": {Uuid: "shared", IsPrivate: &yes, HasReadAccess: 1, CreatedBy: owner},
			"theirs": {Uuid: "theirs", IsPrivate: &yes, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "sana"}},
		},
		"ravi": {
			"open":   {Uuid: "open", IsPrivate: &no},
			"secret": {Uuid: "secret", IsPrivate: &yes, CreatedBy: owner},
			"shared": {Uuid: "shared", IsPrivate: &yes, CreatedBy: owner},
			"theirs": {Uuid: "theirs", IsPrivate: &yes, HasReadAccess: 1, CreatedBy: &dgraphStruct.DgraphUser{Uuid: "sana"}},
		},
	}
	boards := map[string]*dgraphStruct.DgraphBoard{
		"roadmap": {Uuid: "roadmap", IsPrivate: &no},
		"payroll": {Uuid: "payroll", IsPrivate: &yes},
	}
	var docCalls, boardCalls int
	failDocs := false
	restoreDocs, restoreBoards, restorePerson := readableDocs, readableBoards, lookupPerson
	t.Cleanup(func() { readableDocs, readableBoards, lookupPerson = restoreDocs, restoreBoards, restorePerson })
	readableDocs = func(_ context.Context, ids []string, userUUID string) (map[string]bool, error) {
		docCalls++
		if failDocs {
			return nil, errors.New("dgraph is down")
		}
		out := map[string]bool{}
		for _, id := range ids {
			if d, ok := docs[userUUID][id]; ok {
				out[strings.ToLower(id)] = docBusiness.CanRead(d, userUUID)
			}
		}
		return out, nil
	}
	boardReads := readBoardsAs(boards, map[string]int{})
	readableBoards = func(ctx context.Context, ids []string, userUUID string) (map[string]bool, error) {
		boardCalls++
		return boardReads(ctx, ids, userUUID)
	}
	lookupPerson = func(_ context.Context, uuid string) (*dgraphStruct.DgraphUser, error) {
		if uuid == "ravi" {
			return &dgraphStruct.DgraphUser{Uid: "0xravi", Uuid: "ravi"}, nil
		}
		return nil, errors.New("no such person")
	}
	sana := &userModels.UserInfo{}
	sana.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: "0xsana", Uuid: "sana"}
	hits := []ai.SimilarResult{
		{ContentType: "post", ContentUUID: "p1", ChannelUUID: "ch"},
		{ContentType: "doc", ContentUUID: "open"},
		{ContentType: "doc", ContentUUID: "secret"},
		{ContentType: "doc", ContentUUID: "shared"},
		{ContentType: "doc", ContentUUID: "theirs"},
		{ContentType: "doc", ContentUUID: "missing"},
		{ContentType: "comment", ContentUUID: "c1", DocUUID: "secret"},
		{ContentType: "comment", ContentUUID: "c2", DocUUID: "shared"},
		{ContentType: "comment", ContentUUID: "c3", BoardUUID: "roadmap"},
		{ContentType: "comment", ContentUUID: "c4", BoardUUID: "payroll"},
		{ContentType: ai.BoardContentType, ContentUUID: "roadmap"},
	}
	cases := []struct {
		name               string
		ctx                context.Context
		fail               bool
		want               string
		wantDocs, wantBrds int
	}{
		{"the person's own search", ai.WithoutRunRequester(context.Background()), false, "p1,open,shared,theirs,c2,c3,roadmap", 1, 1},
		{"for someone else", ai.WithRunRequester(context.Background(), "ravi", "sana"), false, "p1,open,theirs,c3,roadmap", 2, 2},
		{"the doc read fails", ai.WithoutRunRequester(context.Background()), true, "p1,c3,roadmap", 1, 1},
		{"for someone who can't be resolved", ai.WithRunRequester(context.Background(), "gone", "sana"), false, "p1", 0, 0},
	}
	for _, c := range cases {
		docCalls, boardCalls, failDocs = 0, 0, c.fail
		var kept []string
		for _, h := range keepReadableDocs(c.ctx, sana, hits) {
			kept = append(kept, h.ContentUUID)
		}
		if got := strings.Join(kept, ","); got != c.want {
			t.Errorf("%s: kept %s, want %s", c.name, got, c.want)
		}
		if docCalls != c.wantDocs {
			t.Errorf("%s: %d doc access calls, want %d", c.name, docCalls, c.wantDocs)
		}
		if boardCalls != c.wantBrds {
			t.Errorf("%s: %d board access calls, want %d", c.name, boardCalls, c.wantBrds)
		}
	}
}
