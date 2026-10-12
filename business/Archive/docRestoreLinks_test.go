package business

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
)

// asAdmin is a request's context for an admin.
func asAdmin(id, name string) context.Context {
	var u userModel.UserInfo
	u.UserDgraphInfo = dgraphStruct.DgraphUser{Uuid: id, UserName: name}
	return context.WithValue(context.Background(), helpers.UserInfoContextKey, u)
}

// An admin restores docs; that doesn't make every doc theirs to read. The
// restore list shows a private doc they may not read as "Private doc" and
// whose it is, never its title, by the doc read rule asked for the admin; and
// when the rule can't be asked (the graph didn't answer, or nobody is
// asking), every doc shows so.
func TestTheRestoreListKeepsBackTitlesTheAdminMayNotRead(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	owner := &dgraphStruct.DgraphUser{Uuid: "dana", UserName: "Dana Whitfield"}
	public := &dgraphStruct.DgraphDoc{Uuid: "11111111-1111-4111-8111-111111111111", Title: "Handbook", CreatedBy: owner, DeletedAt: &at, ArchivedAt: &at}
	secret := &dgraphStruct.DgraphDoc{Uuid: "22222222-2222-4222-8222-222222222222", Title: "Salaries", CreatedBy: owner, DeletedAt: &at}
	prevList, prevRead := recentlyDeletedDocs, docsCouldRead
	t.Cleanup(func() { recentlyDeletedDocs, docsCouldRead = prevList, prevRead })
	recentlyDeletedDocs = func(context.Context, string, int, int) ([]*dgraphStruct.DgraphDoc, int64, error) {
		return []*dgraphStruct.DgraphDoc{public, secret}, 2, nil
	}
	var askedFor []string
	readErr := error(nil)
	docsCouldRead = func(_ context.Context, ids []string, user string) (map[string]bool, error) {
		askedFor = append(askedFor, user)
		if readErr != nil {
			return nil, readErr
		}
		return map[string]bool{public.Uuid: true, secret.Uuid: false}, nil
	}
	names := func(ctx context.Context) []string {
		t.Helper()
		items, total, err := getRecentlyArchivedDocs(ctx, 50, 0, "Document")
		if err != nil || total != 2 {
			t.Fatalf("the list: %d, %v", total, err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.Name)
		}
		return out
	}

	if got, want := names(asAdmin("sam", "Sam Rivera")), []string{"Handbook", "Private doc · deleted by Dana Whitfield"}; !slices.Equal(got, want) {
		t.Errorf("to an admin who may read only the handbook the list reads %q, want %q", got, want)
	}
	if len(askedFor) != 1 || askedFor[0] != "sam" {
		t.Errorf("the read rule was asked for %v, want the admin asking", askedFor)
	}

	readErr = errors.New("the graph did not answer")
	if got, want := names(asAdmin("sam", "Sam Rivera")), []string{"Private doc · owned by Dana Whitfield", "Private doc · deleted by Dana Whitfield"}; !slices.Equal(got, want) {
		t.Errorf("when the rule can't be asked the list reads %q, want every doc kept back %q", got, want)
	}
	askedFor = nil
	if got, want := names(context.Background()), []string{"Private doc · owned by Dana Whitfield", "Private doc · deleted by Dana Whitfield"}; !slices.Equal(got, want) || len(askedFor) != 0 {
		t.Errorf("with nobody asking the list reads %q (rule asked for %v), want every doc kept back %q", got, askedFor, want)
	}
}

// stubDocRestore replaces the doc restore's reads and writes, and records
// what it was asked to do, in order.
type stubDocRestore struct {
	docs        []*dgraphStruct.DgraphDoc
	readErr     error
	links       map[string]int64
	linksErr    map[string]error
	steps       []string
	restoreAsks [][]string
	told        []string
}

func (s *stubDocRestore) install(t *testing.T) {
	t.Helper()
	prevRead, prevRestore, prevOff, prevTell, prevSearch := docsDeletion, restoreDocNodes, turnOffDocLinks, tellOwnerLinksOff, docSearchRestore
	t.Cleanup(func() {
		docsDeletion, restoreDocNodes, turnOffDocLinks, tellOwnerLinksOff, docSearchRestore = prevRead, prevRestore, prevOff, prevTell, prevSearch
	})
	docsDeletion = func(context.Context, []string) ([]*dgraphStruct.DgraphDoc, error) {
		s.steps = append(s.steps, "read")
		return s.docs, s.readErr
	}
	turnOffDocLinks = func(_ context.Context, id string) (int64, error) {
		s.steps = append(s.steps, "links off "+id)
		return s.links[id], s.linksErr[id]
	}
	restoreDocNodes = func(_ context.Context, ids []string, _ bool) ([]string, error) {
		s.steps = append(s.steps, "restore")
		s.restoreAsks = append(s.restoreAsks, ids)
		return ids, nil
	}
	docSearchRestore = func([]string) {}
	tellOwnerLinksOff = func(_ context.Context, d *dgraphStruct.DgraphDoc, n int64, byName string) {
		_, body := linksOffNotice(byName, d.Title, n)
		s.told = append(s.told, d.CreatedBy.Uuid+": "+body)
	}
}

// A doc its owner deleted comes back from the restore list with its guest
// links off: dead while it was deleted, they opened it again to guests the
// owner had taken it from. They go before the doc is restored, and the owner
// is told. A doc the retention archived keeps its links, from the list as
// from an undo, and nobody is told anything.
func TestRestoringADocItsOwnerDeletedTurnsOffItsGuestLinks(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	owner := &dgraphStruct.DgraphUser{Uuid: "dana", UserName: "Dana Whitfield"}
	deleted := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	archived := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	noLinks := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	docs := []*dgraphStruct.DgraphDoc{
		{Uuid: deleted.String(), Title: "Q4 plan", CreatedBy: owner, DeletedAt: &at},
		{Uuid: archived.String(), Title: "Old notes", CreatedBy: owner, DeletedAt: &at, ArchivedAt: &at},
		{Uuid: noLinks.String(), Title: "Scratch", CreatedBy: owner, DeletedAt: &at},
	}

	s := &stubDocRestore{docs: docs, links: map[string]int64{deleted.String(): 2}}
	s.install(t)
	n, err := restoreDocs(asAdmin("sam", "Sam Rivera"), []uuid.UUID{deleted, archived, noLinks}, false)
	if err != nil || n != 3 {
		t.Fatalf("restoring from the list: %d, %v", n, err)
	}
	if want := []string{"read", "links off " + deleted.String(), "links off " + noLinks.String(), "restore"}; !slices.Equal(s.steps, want) {
		t.Errorf("the restore went %q, want %q: links off for the docs their owner deleted, before the restore", s.steps, want)
	}
	if len(s.told) != 1 || !strings.HasPrefix(s.told[0], "dana: Sam Rivera restored “Q4 plan”, which you deleted. Its 2 guest links were turned off;") {
		t.Errorf("told %q, want the owner told of the 2 links of the doc they deleted, and nothing of a doc that had none", s.told)
	}

	// Undoing a run brings back only archived docs, and their links with them.
	s = &stubDocRestore{docs: docs}
	s.install(t)
	if _, err := restoreDocs(asAdmin("sam", "Sam Rivera"), []uuid.UUID{archived}, true); err != nil {
		t.Fatal(err)
	}
	if want := []string{"restore"}; !slices.Equal(s.steps, want) || len(s.told) != 0 {
		t.Errorf("undo went %q and told %q, want it to restore and leave the links alone", s.steps, s.told)
	}

	// An owner restoring their own doc is told nothing; its links still go.
	s = &stubDocRestore{docs: docs[:1], links: map[string]int64{deleted.String(): 1}}
	s.install(t)
	if _, err := restoreDocs(asAdmin("dana", "Dana Whitfield"), []uuid.UUID{deleted}, false); err != nil {
		t.Fatal(err)
	}
	if want := []string{"read", "links off " + deleted.String(), "restore"}; !slices.Equal(s.steps, want) || len(s.told) != 0 {
		t.Errorf("the owner's own restore went %q and told %q, want links off and nobody told", s.steps, s.told)
	}
}

// A doc whose links can't be turned off stays deleted, and the restore says
// so; the others still come back. When the docs can't be read to tell which
// their owners deleted, nothing is restored.
func TestADocWhoseLinksStayOnStaysDeleted(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	owner := &dgraphStruct.DgraphUser{Uuid: "dana", UserName: "Dana Whitfield"}
	stuck := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	archived := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	docs := []*dgraphStruct.DgraphDoc{
		{Uuid: stuck.String(), Title: "Q4 plan", CreatedBy: owner, DeletedAt: &at},
		{Uuid: archived.String(), Title: "Old notes", CreatedBy: owner, DeletedAt: &at, ArchivedAt: &at},
	}
	s := &stubDocRestore{docs: docs, linksErr: map[string]error{stuck.String(): errors.New("postgres is down")}}
	s.install(t)
	n, err := restoreDocs(asAdmin("sam", "Sam Rivera"), []uuid.UUID{stuck, archived}, false)
	if err == nil || n != 1 {
		t.Errorf("restoring with one doc's links stuck on: %d, %v; want the other doc restored and an error", n, err)
	}
	if len(s.restoreAsks) != 1 || !slices.Equal(s.restoreAsks[0], []string{archived.String()}) || len(s.told) != 0 {
		t.Errorf("restored %v and told %q, want only the archived doc restored and nobody told", s.restoreAsks, s.told)
	}

	s = &stubDocRestore{docs: docs, readErr: errors.New("the graph did not answer")}
	s.install(t)
	if n, err := restoreDocs(asAdmin("sam", "Sam Rivera"), []uuid.UUID{stuck, archived}, false); err == nil || n != 0 || len(s.restoreAsks) != 0 {
		t.Errorf("restoring docs that can't be read: %d, %v, restore asked %v; want nothing restored and an error", n, err, s.restoreAsks)
	}
}

// What a doc's owner is told, whoever restored it and however many links.
func TestTheOwnerIsToldWhoRestoredTheDocAndThatItsLinksAreOff(t *testing.T) {
	for _, c := range []struct {
		by, title string
		links     int64
		want      string
	}{
		{"Sam Rivera", "Q4 plan", 2, "Sam Rivera restored “Q4 plan”, which you deleted. Its 2 guest links were turned off; you can turn guest access back on by sharing it again."},
		{"", " ", 1, "An admin restored “Untitled document”, which you deleted. Its guest link was turned off; you can turn guest access back on by sharing it again."},
	} {
		heading, body := linksOffNotice(c.by, c.title, c.links)
		if heading != "Your doc was restored" || body != c.want {
			t.Errorf("%q, %q", heading, body)
		}
	}
}
