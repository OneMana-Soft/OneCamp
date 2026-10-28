package docController

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// fixedRules answers one decision, or one error, for every doc.
type fixedRules struct {
	accessBusiness.Resolver
	d   accessBusiness.Decision
	err error
}

func (f fixedRules) Resolve(context.Context, accessBusiness.Subject, accessBusiness.Resource) (accessBusiness.Decision, error) {
	return f.d, f.err
}

func askSession(t *testing.T, rules accessBusiness.Resolver) (int, map[string]any) {
	t.Helper()
	old := accessBusiness.Rules
	accessBusiness.Rules = rules
	t.Cleanup(func() { accessBusiness.Rules = old })
	oldName := presenceName
	presenceName = func(context.Context, uuid.UUID) string { return "Sam Rivera" }
	t.Cleanup(func() { presenceName = oldName })
	r := httptest.NewRequest(http.MethodPost, "/docColab/session", strings.NewReader(`{"doc_uuid":"doc-1"}`))
	who := userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "u-1"}}
	r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, who))
	w := httptest.NewRecorder()
	DocCollabSession(w, r)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

// Everyone who may read joins live; only edit and full may write. Before the
// session, a reader or commenter was refused and read a body that never moved.
func TestDocSessionAnswersEveryReaderWithTheirLevel(t *testing.T) {
	cases := []struct {
		level   accessBusiness.Level
		canEdit bool
	}{
		{accessBusiness.LevelView, false},
		{accessBusiness.LevelComment, false},
		{accessBusiness.LevelEdit, true},
		{accessBusiness.LevelFull, true},
	}
	for _, c := range cases {
		code, body := askSession(t, fixedRules{d: accessBusiness.Decision{Level: c.level}})
		if code != http.StatusOK {
			t.Fatalf("%s: answered %d, want 200", c.level, code)
		}
		// And who opens it, as their presence names them (presenceName).
		if body["level"] != c.level.String() || body["canEdit"] != c.canEdit || body["locked"] != false || body["name"] != "Sam Rivera" {
			t.Errorf("%s: answered %v", c.level, body)
		}
	}
}

func TestDocSessionRefusesWhoMayNotRead(t *testing.T) {
	if code, _ := askSession(t, fixedRules{d: accessBusiness.Decision{Level: accessBusiness.LevelNone}}); code != http.StatusForbidden {
		t.Errorf("no access answered %d, want 403", code)
	}
	if code, _ := askSession(t, fixedRules{err: business.ErrNotFound}); code != http.StatusNotFound {
		t.Errorf("no such doc answered %d, want 404", code)
	}
}

// Nobody joins a deleted doc live, its owner included; a stranger learns
// nothing (404).
func TestDocSessionRefusesADeletedDoc(t *testing.T) {
	if code, _ := askSession(t, fixedRules{d: accessBusiness.Decision{Level: accessBusiness.LevelFull, Owner: true, Deleted: true}}); code != http.StatusGone {
		t.Errorf("the owner of a deleted doc got %d, want 410", code)
	}
	if code, _ := askSession(t, fixedRules{d: accessBusiness.Decision{Deleted: true}}); code != http.StatusNotFound {
		t.Errorf("a stranger to a deleted doc got %d, want 404", code)
	}
	if code, _ := askSession(t, fixedRules{err: business.ErrDocDeleted}); code != http.StatusGone {
		t.Errorf("ErrDocDeleted answered %d, want 410", code)
	}
}

// A graph that didn't answer is not "no access": the service tries again.
func TestDocSessionSaysTryAgainWhenAccessCantBeChecked(t *testing.T) {
	if code, _ := askSession(t, fixedRules{err: context.DeadlineExceeded}); code != http.StatusServiceUnavailable {
		t.Errorf("a failed check answered %d, want 503", code)
	}
}

// A hard lock leaves only full access able to write (D-12).
func TestDocSessionHardLockLeavesOnlyFullAccessWriting(t *testing.T) {
	if s := sessionFor(accessBusiness.Decision{Level: accessBusiness.LevelEdit, Locked: true}); s.CanEdit || !s.Locked {
		t.Errorf("an editor of a locked doc: %+v", s)
	}
	if s := sessionFor(accessBusiness.Decision{Level: accessBusiness.LevelFull, Locked: true}); !s.CanEdit {
		t.Errorf("full access on a locked doc: %+v", s)
	}
}
