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

// An edit (POST /doc/updateDoc) decides through accessBusiness.Open before it
// reads or says anything about the doc: a doc the person can't open answers
// exactly as one that isn't there (404, helpers.DocNotFound);
// one in the trash is 410 to the people it was there for; one they may only
// read is 403; a hard lock says so (423); and a decision that couldn't be made
// is a read that failed (503), never a no. It used to read the doc first and
// answer a doc it couldn't open with 403, which said the doc was there.
func TestAnEditDecidesBeforeItSaysAnything(t *testing.T) {
	const body = `{"doc_uuid":"d0c00000-0000-4000-8000-000000000001","doc_title":"Plans"}`
	edit := func(rules accessBusiness.Resolver) (int, string) {
		t.Helper()
		old := accessBusiness.Rules
		accessBusiness.Rules = rules
		defer func() { accessBusiness.Rules = old }()
		r := httptest.NewRequest(http.MethodPost, "/doc/updateDoc", strings.NewReader(body))
		who := userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "a11ce000-0000-4000-8000-000000000001"}}
		r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, who))
		w := httptest.NewRecorder()
		UpdateDocBodyLive(w, r)
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	missingCode, missingBody := edit(fixedRules{err: business.ErrNotFound})
	if missingCode != http.StatusNotFound || !strings.Contains(missingBody, helpers.DocNotFound) {
		t.Fatalf("a doc that isn't there: %d %s, want 404 %q", missingCode, missingBody, helpers.DocNotFound)
	}
	view := accessBusiness.Decision{Level: accessBusiness.LevelView, Unlocked: accessBusiness.LevelView, Was: accessBusiness.LevelView}
	locked := accessBusiness.Decision{Level: accessBusiness.LevelComment, Unlocked: accessBusiness.LevelEdit, Was: accessBusiness.LevelComment, Locked: true, Lock: "hard"}
	for _, c := range []struct {
		name  string
		rules fixedRules
		want  int
	}{
		{"in the trash, to someone it was there for", fixedRules{d: accessBusiness.Decision{Deleted: true, Was: accessBusiness.LevelView}}, http.StatusGone},
		{"theirs to read", fixedRules{d: view}, http.StatusForbidden},
		{"hard-locked for an editor", fixedRules{d: locked}, http.StatusLocked},
		{"a decision that couldn't be made", fixedRules{err: errors.New("graph down")}, http.StatusServiceUnavailable},
	} {
		if code, got := edit(c.rules); code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, got, c.want)
		}
	}
	for _, c := range []struct {
		name  string
		rules fixedRules
	}{
		{"not theirs to open", fixedRules{}},
		{"in the trash, to someone it wasn't there for", fixedRules{d: accessBusiness.Decision{Deleted: true}}},
	} {
		if code, got := edit(c.rules); code != missingCode || got != missingBody {
			t.Errorf("%s: %d %s, where a doc that isn't there answers %d %s", c.name, code, got, missingCode, missingBody)
		}
	}
}
