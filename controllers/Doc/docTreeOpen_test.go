package docController

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// The tree's handlers decide a page through accessBusiness.Open, so they
// refuse it alike: a page in the trash is 410 to everyone it was there for,
// not only its owner, and 404 to anyone else; a page someone can't open is
// 404; one they may only view is 403 where they'd change it; and a hard lock
// says so (423). Each refuses before reading anything else, so the resolver
// alone answers here.
func TestTheTreeRefusesAPageAsOpenDecides(t *testing.T) {
	const page = "d0c00000-0000-4000-8000-000000000001"
	handlers := []struct {
		name string
		run  http.HandlerFunc
		body string
	}{
		{"create under it", CreateDocInTree, `{"doc_title":"Notes","parent_uuid":"` + page + `"}`},
		{"delete it", DeleteDocTree, `{"doc_uuid":"` + page + `"}`},
		{"copy it", DuplicateDoc, `{"doc_uuid":"` + page + `"}`},
		{"preview moving it", PreviewDocMove, `{"doc_uuid":"` + page + `"}`},
	}
	view := accessBusiness.Decision{Level: accessBusiness.LevelView, Unlocked: accessBusiness.LevelView, Was: accessBusiness.LevelView}
	locked := accessBusiness.Decision{Level: accessBusiness.LevelComment, Unlocked: accessBusiness.LevelEdit, Was: accessBusiness.LevelComment, Locked: true, Lock: "hard"}
	for _, c := range []struct {
		name  string
		rules fixedRules
		// want is each handler's answer, in handlers' order; 0 is a page the
		// handler may go on with (copying needs only view).
		want [4]int
	}{
		{"in the trash, to an editor who isn't its owner", fixedRules{d: accessBusiness.Decision{Deleted: true, Was: accessBusiness.LevelEdit}}, [4]int{410, 410, 410, 410}},
		{"in the trash, to someone it wasn't there for", fixedRules{d: accessBusiness.Decision{Deleted: true}}, [4]int{404, 404, 404, 404}},
		{"not there", fixedRules{err: business.ErrNotFound}, [4]int{404, 404, 404, 404}},
		{"not theirs to open", fixedRules{}, [4]int{404, 404, 404, 404}},
		{"theirs to view", fixedRules{d: view}, [4]int{403, 403, 0, 403}},
		{"hard-locked for an editor", fixedRules{d: locked}, [4]int{423, 423, 0, 423}},
		// A decision that couldn't be made is a read that failed, to try
		// again: never "something went wrong" (500), and never a no.
		{"a decision that couldn't be made", fixedRules{err: errors.New("graph down")}, [4]int{503, 503, 503, 503}},
	} {
		for i, h := range handlers {
			if c.want[i] == 0 {
				continue
			}
			if w := decidedBy(t, c.rules, false, h.run, h.body); w.Code != c.want[i] {
				t.Errorf("%s, %s: %d %s, want %d", h.name, c.name, w.Code, strings.TrimSpace(w.Body.String()), c.want[i])
			}
		}
	}
}

// The trash's restore and delete for good decide first, through
// accessBusiness.OpenDeleted: to anyone a page was never there for, both
// answer as a missing page does (404), live, in the trash, archived or not
// there at all, before reading anything else. Someone it was there for hears
// that a live one isn't in the trash (409), or that the level they had falls
// short (403). A workspace admin may do either to any page there is, and a
// decision that couldn't be made is a read that failed (503), never a yes or
// a no.
func TestTheTrashDecidesBeforeItSaysAnything(t *testing.T) {
	const body = `{"doc_uuid":"d0c00000-0000-4000-8000-000000000001"}`
	handlers := []struct {
		name string
		run  http.HandlerFunc
	}{
		{"restore it", RestoreDocFromTrash},
		{"delete it for good", PurgeDocForGood},
	}
	deleted := func(was accessBusiness.Level) accessBusiness.Decision {
		return accessBusiness.Decision{Deleted: true, Was: was}
	}
	view := accessBusiness.Decision{Level: accessBusiness.LevelView, Unlocked: accessBusiness.LevelView, Was: accessBusiness.LevelView}
	for _, c := range []struct {
		name  string
		rules fixedRules
		admin bool
		// want is each handler's answer, in handlers' order; 0 is a page the
		// handler may go on with.
		want [2]int
	}{
		{"live, to someone it isn't there for", fixedRules{}, false, [2]int{404, 404}},
		{"in the trash, to someone it wasn't there for", fixedRules{d: deleted(accessBusiness.LevelNone)}, false, [2]int{404, 404}},
		{"archived, to someone it wasn't there for", fixedRules{d: accessBusiness.Decision{Deleted: true, Archived: true}}, false, [2]int{404, 404}},
		{"not there", fixedRules{err: business.ErrNotFound}, false, [2]int{404, 404}},
		{"not there, to an admin", fixedRules{err: business.ErrNotFound}, true, [2]int{404, 404}},
		{"live, to someone who can open it", fixedRules{d: view}, false, [2]int{409, 409}},
		{"live, to an admin", fixedRules{}, true, [2]int{409, 409}},
		{"in the trash, to someone who could only read it", fixedRules{d: deleted(accessBusiness.LevelView)}, false, [2]int{403, 403}},
		{"in the trash, to an editor", fixedRules{d: deleted(accessBusiness.LevelEdit)}, false, [2]int{0, 403}},
		{"in the trash, to full access", fixedRules{d: deleted(accessBusiness.LevelFull)}, false, [2]int{0, 0}},
		{"in the trash, to an admin it was never there for", fixedRules{d: deleted(accessBusiness.LevelNone)}, true, [2]int{0, 0}},
		{"a decision that couldn't be made", fixedRules{err: errors.New("graph down")}, false, [2]int{503, 503}},
	} {
		for i, h := range handlers {
			if c.want[i] == 0 {
				continue
			}
			if w := decidedBy(t, c.rules, c.admin, h.run, body); w.Code != c.want[i] {
				t.Errorf("%s, %s: %d %s, want %d", h.name, c.name, w.Code, strings.TrimSpace(w.Body.String()), c.want[i])
			}
		}
	}
}

// decidedBy is a handler's answer to body, sent by a member (a workspace
// admin when admin), with rules deciding every page.
func decidedBy(t *testing.T, rules accessBusiness.Resolver, admin bool, run http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	old := accessBusiness.Rules
	accessBusiness.Rules = rules
	defer func() { accessBusiness.Rules = old }()
	r := httptest.NewRequest(http.MethodPost, "/doc", strings.NewReader(body))
	who := userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "a11ce000-0000-4000-8000-000000000001"}}
	who.UserPostgresInfo.IsAdmin = admin
	r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, who))
	w := httptest.NewRecorder()
	run(w, r)
	return w
}
