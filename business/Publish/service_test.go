package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	attachmentModels "github.com/akashc777/OneCamp/models/postgres/Attachment"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// world is the grants, the doc and everything the service reads, in memory.
// Its resolver keeps business/Guest's rules (ResolveExternal, tested there):
// the guest switch and its publishing sub-option, revoked, expired, live doc.
type world struct {
	t         *testing.T
	grants    map[uuid.UUID]*guestModel.GuestGrant
	tokens    map[string]uuid.UUID
	docs      map[string]*dgraphStruct.DgraphDoc
	files     map[string]string // file → its doc
	guestOn   bool
	publishOn bool
	cache     map[string]cached
	kept      int
	reads     int
	work      map[string]int
	busy      bool
	tries     []string
	unlockNo  bool
	added     []string
	seeded    map[string]string
	madeFor   string
	clock     time.Time
}

var (
	viewID    = uuid.MustParse("e0000000-0000-4000-8000-0000000000e1")
	commentID = uuid.MustParse("e0000000-0000-4000-8000-0000000000e2")
)

func newWorld(t *testing.T) *world {
	t.Helper()
	t.Setenv("JWT_SECRET", "test-secret-for-unlocks")
	w := &world{
		t: t, grants: map[uuid.UUID]*guestModel.GuestGrant{}, tokens: map[string]uuid.UUID{}, docs: map[string]*dgraphStruct.DgraphDoc{},
		files: map[string]string{}, guestOn: true, publishOn: true, cache: map[string]cached{}, work: map[string]int{},
		seeded: map[string]string{}, clock: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC),
	}
	updated := w.clock.Add(-time.Hour)
	zero := time.Time{}
	w.docs[docID] = &dgraphStruct.DgraphDoc{Uuid: docID, Title: "Launch notes", UpdatedAt: &updated, DeletedAt: &zero,
		Body: `<p>Hello <span data-type="mention" data-id="uid-1">@Priya</span></p><img src="` + att(docID, fileA) + `" alt="Roadmap">` +
			`<img src="` + att(otherID, fileB) + `" alt="Theirs"><div data-type="table-embed" data-table="t-1"></div>`}
	w.files[fileA] = docID
	w.files[fileB] = otherID
	slug := "launch-notes"
	w.grants[viewID] = &guestModel.GuestGrant{Id: viewID, ResourceType: "doc", ResourceID: docID, Capability: "view", UnlockVersion: 1, Slug: &slug}
	w.grants[commentID] = &guestModel.GuestGrant{Id: commentID, ResourceType: "doc", ResourceID: docID, Capability: "comment", UnlockVersion: 1}
	w.tokens["view-token"] = viewID
	w.tokens["comment-token"] = commentID

	oldResolve, oldRead, oldLive, oldNow := resolve, readDoc, liveGrantsOf, now
	oldPage, oldUnlock, oldCached, oldKeep, oldDrop := pageAllowed, unlockAllowed, cachedPage, keepPage, dropPage
	oldFile, oldWeb, oldAPI, oldNew, oldAdd, oldSeed := fileOfDoc, webBase, apiBase, newPrivateDoc, addFile, seedCopy
	t.Cleanup(func() {
		resolve, readDoc, liveGrantsOf, now = oldResolve, oldRead, oldLive, oldNow
		pageAllowed, unlockAllowed, cachedPage, keepPage, dropPage = oldPage, oldUnlock, oldCached, oldKeep, oldDrop
		fileOfDoc, webBase, apiBase, newPrivateDoc, addFile, seedCopy = oldFile, oldWeb, oldAPI, oldNew, oldAdd, oldSeed
	})
	resolve = func(_ context.Context, ref Ref) (*guestModel.GuestGrant, error) {
		if !w.guestOn {
			return nil, guestBusiness.ErrGuestDisabled
		}
		var g *guestModel.GuestGrant
		if ref.Slug != "" {
			if !w.publishOn {
				return nil, guestBusiness.ErrGuestDisabled
			}
			for _, x := range w.grants {
				if x.Slug != nil && *x.Slug == ref.Slug && x.RevokedAt == nil {
					g = x
				}
			}
			if g != nil && g.ExpiresAt != nil && !g.ExpiresAt.After(w.clock) {
				return nil, guestBusiness.ErrGone
			}
		} else if id, ok := w.tokens[ref.Token]; ok {
			g = w.grants[id]
		}
		if g == nil || g.RevokedAt != nil || (g.ExpiresAt != nil && !g.ExpiresAt.After(w.clock)) {
			return nil, guestBusiness.ErrInvalidGrant
		}
		if d := w.docs[g.ResourceID]; d == nil || (d.DeletedAt != nil && !d.DeletedAt.IsZero()) {
			return nil, guestBusiness.ErrNotFound
		}
		copied := *g
		return &copied, nil
	}
	readDoc = func(_ context.Context, id string) (*dgraphStruct.DgraphDoc, error) {
		w.reads++
		if d, ok := w.docs[id]; ok {
			return d, nil
		}
		return nil, errors.New("no such doc")
	}
	liveGrantsOf = func(_ context.Context, doc string) ([]*guestModel.GuestGrant, error) {
		var out []*guestModel.GuestGrant
		for _, g := range w.grants {
			if g.ResourceID == doc && g.RevokedAt == nil {
				out = append(out, g)
			}
		}
		return out, nil
	}
	now = func() time.Time { return w.clock }
	pageAllowed = func(_ context.Context, grant string) bool { w.work[grant]++; return !w.busy }
	unlockAllowed = func(_ context.Context, ip string, g *guestModel.GuestGrant) bool {
		w.tries = append(w.tries, ip+"|"+g.Id.String())
		return !w.unlockNo
	}
	cachedPage = func(_ context.Context, grant string) (*cached, bool) {
		c, ok := w.cache[grant]
		return &c, ok
	}
	keepPage = func(_ context.Context, grant string, c cached) { w.kept++; w.cache[grant] = c }
	dropPage = func(_ context.Context, grant string) { delete(w.cache, grant) }
	fileOfDoc = func(_ context.Context, doc, obj string) (*attachmentModels.Attachment, error) {
		if w.files[obj] != doc {
			return nil, fileAccessBusiness.ErrNoSuchFile
		}
		return &attachmentModels.Attachment{Uuid: uuid.MustParse(obj), ObjKey: "u1/" + obj + "_picture.png", SrcKey: "doc", SrcValue: doc}, nil
	}
	webBase = func() string { return "https://app.example.test" }
	apiBase = func() string { return "https://api.example.test" }
	newPrivateDoc = func(_ context.Context, by *dgraphStruct.DgraphUser, title string) (string, error) {
		w.madeFor = by.Uuid + "|" + title
		return "c0000000-0000-4000-8000-0000000000cc", nil
	}
	addFile = func(_ context.Context, _ uuid.UUID, objKey string, _ uuid.UUID, doc string) error {
		w.added = append(w.added, doc+":"+objKey)
		return nil
	}
	seedCopy = func(_ context.Context, doc, body, _ string) error { w.seeded[doc] = body; return nil }
	return w
}

func (w *world) password(p string) {
	w.t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.MinCost)
	if err != nil {
		w.t.Fatal(err)
	}
	s := string(h)
	w.grants[viewID].PasswordHash = &s
}

var (
	bySlugRef  = Ref{Slug: "launch-notes"}
	byTokenRef = Ref{Token: "view-token"}
)

// A doc's view link is a page by its token and by its address; a comment
// link opens the app, never a page; nothing else is one.
func TestOnlyALiveDocsViewLinkIsAPage(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for name, ref := range map[string]Ref{"the address": bySlugRef, "the token": byTokenRef} {
		p, err := PageFor(ctx, ref, "")
		if err != nil || p.Title != "Launch notes" || p.Slug != "launch-notes" {
			t.Fatalf("%s: %+v, %v", name, p, err)
		}
		if strings.Contains(p.HTML, "uid-1") || strings.Contains(p.HTML, otherID) || strings.Contains(p.HTML, "t-1") ||
			!strings.Contains(p.HTML, PagePath(ref)+"/files/"+fileA+`"`) {
			t.Errorf("%s: %s", name, p.HTML)
		}
	}
	for name, ref := range map[string]Ref{
		"a comment link": {Token: "comment-token"}, "an unknown token": {Token: "nope"}, "an unknown address": {Slug: "nope"},
	} {
		if _, err := PageFor(ctx, ref, ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	trashed := time.Now()
	w.docs[docID].DeletedAt = &trashed
	if _, err := PageFor(ctx, byTokenRef, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("a doc in the trash: %v", err)
	}
}

// One switch: guest access off stops a link's page and its published address
// at once; its one sub-option, publishing, stops the address alone.
func TestOneSwitchStopsEveryPage(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.guestOn = false
	for _, ref := range []Ref{bySlugRef, byTokenRef} {
		if _, err := PageFor(ctx, ref, ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("%+v with guest access off: %v", ref, err)
		}
		if _, err := FileFor(ctx, ref, fileA, ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("%+v's picture with guest access off: %v", ref, err)
		}
	}
	w.guestOn, w.publishOn = true, false
	if _, err := PageFor(ctx, bySlugRef, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("an address with publishing off: %v", err)
	}
	if _, err := PageFor(ctx, byTokenRef, ""); err != nil {
		t.Errorf("a link's page with publishing off: %v", err)
	}
}

// A revoked link's page ends at once, whatever a cache still holds (D-25,
// condition 5); an expired address says it expired.
func TestRevokingEndsThePageAtOnce(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := PageFor(ctx, bySlugRef, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.cache[viewID.String()]; !ok {
		t.Fatal("the published page wasn't kept")
	}
	at := time.Now()
	w.grants[viewID].RevokedAt = &at
	for _, ref := range []Ref{bySlugRef, byTokenRef} {
		if _, err := PageFor(ctx, ref, ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("%+v after a revoke: %v", ref, err)
		}
		if _, err := FileFor(ctx, ref, fileA, ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("%+v's picture after a revoke: %v", ref, err)
		}
	}
	w.grants[viewID].RevokedAt = nil
	past := w.clock.Add(-time.Minute)
	w.grants[viewID].ExpiresAt = &past
	if _, err := PageFor(ctx, bySlugRef, ""); !errors.Is(err, ErrGone) {
		t.Errorf("an expired address: %v", err)
	}
	if _, err := PageFor(ctx, byTokenRef, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("an expired token stays not found: %v", err)
	}
}

// The address's page is kept and its copy never counted; a token's page and
// a password page are never kept; a token's page is never found by search
// engines.
func TestKeepingAndCountingAPage(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.grants[viewID].Indexable = true
	first, err := PageFor(ctx, bySlugRef, "")
	if err != nil || !first.Indexable {
		t.Fatalf("%+v, %v", first, err)
	}
	w.busy = true
	second, err := PageFor(ctx, bySlugRef, "")
	if err != nil || second.HTML != first.HTML || !second.Indexable || second.Slug != "launch-notes" {
		t.Errorf("the kept copy: %+v, %v", second, err)
	}
	if w.work[viewID.String()] != 1 || w.reads != 1 || w.kept != 1 {
		t.Errorf("work %d, reads %d, kept %d", w.work[viewID.String()], w.reads, w.kept)
	}
	w.busy = false
	token, err := PageFor(ctx, byTokenRef, "")
	if err != nil || token.Indexable || w.kept != 1 {
		t.Errorf("a token's page: indexable %v, kept %d, %v", token.Indexable, w.kept, err)
	}
	w.grants[viewID].AllowCopies = true
	if w.cache[viewID.String()].fits(w.grants[viewID], nil) {
		t.Error("a kept copy fits a grant that changed")
	}
	delete(w.cache, viewID.String())
	w.busy = true
	if _, err := PageFor(ctx, bySlugRef, ""); !errors.Is(err, ErrBusy) {
		t.Errorf("a busy page worked out afresh: %v", err)
	}
}

// A password opens the page by either way in; a new password or address
// ends every unlock before it; tries are counted per address and link.
func TestAPasswordOpensThePage(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.password("open sesame")
	for _, ref := range []Ref{bySlugRef, byTokenRef} {
		if _, err := PageFor(ctx, ref, ""); !errors.Is(err, ErrLocked) {
			t.Errorf("%+v without its unlock: %v", ref, err)
		}
	}
	if _, _, err := Unlock(ctx, bySlugRef, "wrong", "203.0.113.9"); !errors.Is(err, ErrWrongPassword) {
		t.Errorf("a wrong password: %v", err)
	}
	token, lasts, err := Unlock(ctx, byTokenRef, "open sesame", "2001:db8:1:2:3:4:5:6")
	if err != nil || lasts != guestBusiness.KindPage.UnlockFor {
		t.Fatal(err)
	}
	if last := w.tries[len(w.tries)-1]; last != "2001:db8:1:2::/64|"+viewID.String() {
		t.Errorf("tries are counted per address (an IPv6 one by its /64) and link: %s", last)
	}
	for _, ref := range []Ref{bySlugRef, byTokenRef} {
		p, err := PageFor(ctx, ref, token)
		if err != nil || p.Token != token || !strings.Contains(p.HTML, fileA+"?k="+token) {
			t.Errorf("%+v unlocked: %+v, %v", ref, p, err)
		}
		if f, err := FileFor(ctx, ref, fileA, token); err != nil || !f.Private {
			t.Errorf("%+v's picture unlocked: %+v, %v", ref, f, err)
		}
		if _, err := FileFor(ctx, ref, fileA, ""); !errors.Is(err, ErrLocked) {
			t.Errorf("%+v's picture without its unlock: %v", ref, err)
		}
	}
	if w.kept != 0 {
		t.Error("a password page was kept")
	}
	w.grants[viewID].UnlockVersion++
	if _, err := PageFor(ctx, bySlugRef, token); !errors.Is(err, ErrLocked) {
		t.Errorf("an unlock outlives a new password: %v", err)
	}
	w.unlockNo = true
	if _, _, err := Unlock(ctx, bySlugRef, "open sesame", "203.0.113.9"); !errors.Is(err, ErrBusy) {
		t.Errorf("too many tries: %v", err)
	}
	if _, _, err := Unlock(ctx, Ref{Token: "comment-token"}, "open sesame", "203.0.113.9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a comment link's unlock: %v", err)
	}
}

// A page's files are only its own doc's (D-25, condition 3).
func TestAPagesFilesAreOnlyItsDocs(t *testing.T) {
	newWorld(t)
	ctx := context.Background()
	for _, ref := range []Ref{bySlugRef, byTokenRef} {
		f, err := FileFor(ctx, ref, strings.ToUpper(fileA), "")
		if err != nil || f.ObjKey != "u1/"+fileA+"_picture.png" || f.ETag != `"`+fileA+`"` || f.Private {
			t.Errorf("%+v: the doc's own file: %+v, %v", ref, f, err)
		}
		if _, err := FileFor(ctx, ref, fileB, ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("%+v: another doc's file under this page: %v", ref, err)
		}
		if _, err := FileFor(ctx, ref, "not-a-file", ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("%+v: an id that isn't one: %v", ref, err)
		}
	}
	if _, err := FileFor(ctx, Ref{Token: "comment-token"}, fileA, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("a comment link's file through a page: %v", err)
	}
}

// A copy is the person's own, only when the link lets copies, never an agent's.
func TestACopyIsThePersonsOwn(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	me := &dgraphStruct.DgraphUser{Uid: "0x09", Uuid: "a0000000-0000-4000-8000-000000000002"}
	if _, err := Duplicate(ctx, me, "", bySlugRef, ""); !errors.Is(err, ErrNoCopies) {
		t.Errorf("a page that may not be copied: %v", err)
	}
	w.grants[viewID].AllowCopies = true
	if _, err := Duplicate(ctx, me, "b0000000-0000-4000-8000-0000000000b7", bySlugRef, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent copies: %v", err)
	}
	id, err := Duplicate(ctx, me, "", bySlugRef, "")
	if err != nil || id != "c0000000-0000-4000-8000-0000000000cc" || w.madeFor != me.Uuid+"|Launch notes" {
		t.Fatalf("copy %q (%s): %v", id, w.madeFor, err)
	}
	if len(w.added) != 1 || w.added[0] != id+":u1/"+fileA+"_picture.png" {
		t.Errorf("files given to the copy: %v", w.added)
	}
	body := w.seeded[id]
	if !strings.Contains(body, "https://api.example.test/doc/getDocAttachment/"+id+"/") || strings.Contains(body, docID) ||
		strings.Contains(body, otherID) || strings.Contains(body, "table-embed") || strings.Contains(body, "uid-1") {
		t.Errorf("the copy's body: %s", body)
	}
	w.password("open sesame")
	if _, err := Duplicate(ctx, me, "", bySlugRef, ""); !errors.Is(err, ErrLocked) {
		t.Errorf("a password page copied without its unlock: %v", err)
	}
}

// The page wears the doc's look: an emoji icon, its cover, font and width.
func TestThePageWearsItsDocsLook(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	d := w.docs[docID]
	fullWidth := true
	d.Icon, d.CoverKey, d.Font, d.FullWidth = "🚀", docID+"/"+strings.ToUpper(fileA), "mono", &fullWidth
	pg, err := PageFor(ctx, byTokenRef, "")
	if err != nil || pg.Icon != "🚀" || pg.CoverURL != PagePath(byTokenRef)+"/files/"+fileA || pg.Font != "mono" || !pg.FullWidth {
		t.Errorf("look: %+v, %v", pg, err)
	}
	d.Icon, d.CoverKey = "glyph:rocket", otherID+"/"+fileB
	if pg, _ := PageFor(ctx, byTokenRef, ""); pg.Icon != "" || pg.CoverURL != "" || pg.Cover != "" {
		t.Errorf("a glyph, another doc's cover: %+v", pg)
	}
	d.CoverKey = "gallery:dusk"
	if pg, _ := PageFor(ctx, byTokenRef, ""); pg.Cover != "dusk" || pg.CoverURL != "" {
		t.Errorf("a gallery cover: %+v", pg)
	}
}

// A doc's change drops its published pages' copies.
func TestADocsChangesReachItsPages(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	ev := func(k resourceEventBusiness.Kind) resourceEventBusiness.Event {
		return resourceEventBusiness.Event{Kind: k, ResourceKind: "doc", ResourceUUID: docID}
	}
	for _, k := range []resourceEventBusiness.Kind{resourceEventBusiness.DocUpdated, resourceEventBusiness.DocRenamed,
		resourceEventBusiness.DocRestyled, resourceEventBusiness.DocDeleted, resourceEventBusiness.DocRestored} {
		if !wantsPageEvent(ev(k)) {
			t.Errorf("%s doesn't reach the page", k)
		}
	}
	if wantsPageEvent(ev(resourceEventBusiness.DocShared)) || wantsPageEvent(resourceEventBusiness.Event{Kind: resourceEventBusiness.DocUpdated, ResourceKind: "board"}) {
		t.Error("an event that changes no page reaches one")
	}
	w.cache[viewID.String()] = cached{DocUUID: docID, Version: 1, Slug: "launch-notes"}
	onPageEvent(ctx, ev(resourceEventBusiness.DocUpdated))
	if _, ok := w.cache[viewID.String()]; ok {
		t.Error("a save leaves the page's copy")
	}
}

func TestAddresses(t *testing.T) {
	newWorld(t)
	if PagePath(Ref{Slug: "a-b"}) != "/public/p/a-b" || PagePath(Ref{Token: "x/y"}) != "/public/d/x%2Fy" {
		t.Errorf("paths %q %q", PagePath(Ref{Slug: "a-b"}), PagePath(Ref{Token: "x/y"}))
	}
	if WebAddress(Ref{Slug: "a-b"}, "unlock=1") != "https://app.example.test/p/a-b?unlock=1" ||
		WebAddress(Ref{Token: "tok"}, "") != "https://app.example.test/guest/d/tok" {
		t.Error("web addresses")
	}
	if Origin() != "https://api.example.test" {
		t.Errorf("origin %q", Origin())
	}
}
