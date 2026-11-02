package Calendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// artefactKeysToChange is the calendar answering only who may change an
// event's call's artefacts.
type artefactKeysToChange struct {
	business.Service
	keys func(who *model.UserInfo, id string) (business.CallKeys, error)
}

func (f artefactKeysToChange) CallArtefactKeysToChange(_ context.Context, who *model.UserInfo, id string) (business.CallKeys, error) {
	return f.keys(who, id)
}

// Only whoever may change an event deletes what its call made, and brings it
// back (meet-02), by the calendar's own rule, as its call is given and taken
// off: a guest who may only read the event is refused as any change is,
// anyone else is told there is no such event, and a recording filed under
// another event is not found. The delete is soft, and its Undo is the
// restore.
func TestOnlyWhoMayChangeAnEventDeletesItsCallsRecordings(t *testing.T) {
	ev, jonas, maya, lee := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	wasEvents, wasDelete, wasRestore := business.Events, deleteEventArtefact, restoreEventArtefact
	t.Cleanup(func() { business.Events, deleteEventArtefact, restoreEventArtefact = wasEvents, wasDelete, wasRestore })
	business.Events = artefactKeysToChange{keys: func(who *model.UserInfo, id string) (business.CallKeys, error) {
		switch {
		case id == "e1" && who.UserPostgresInfo.Id == jonas:
			return business.CallKeys{Events: []uuid.UUID{ev}}, nil
		case id == "e1" && who.UserPostgresInfo.Id == maya:
			return business.CallKeys{}, business.ErrOnlyCreatorChanges
		}
		return business.CallKeys{}, business.ErrEventNotFound
	}}
	deleted := map[string]bool{}
	change := func(to bool) func(context.Context, business.CallKeys, string) (bool, error) {
		return func(_ context.Context, keys business.CallKeys, key string) (bool, error) {
			if key != "EG_1" || !slices.Contains(keys.Events, ev) || deleted[key] == to {
				return false, nil
			}
			deleted[key] = to
			return true, nil
		}
	}
	deleteEventArtefact, restoreEventArtefact = change(true), change(false)
	ask := func(method string, who uuid.UUID, key string) int {
		path, handler := "/calendar/events/e1/call/artefacts/"+key, DeleteCalendarEventCallArtefact
		if method == http.MethodPost {
			path, handler = path+"/restore", RestoreCalendarEventCallArtefact
		}
		r := httptest.NewRequest(method, path, nil)
		rc := chi.NewRouteContext()
		rc.URLParams.Add("eventId", "e1")
		rc.URLParams.Add("key", key)
		info := model.UserInfo{}
		info.UserPostgresInfo.Id, info.UserDgraphInfo.Uuid = who, who.String()
		ctx := context.WithValue(context.WithValue(r.Context(), chi.RouteCtxKey, rc), helpers.UserInfoContextKey, info)
		w := httptest.NewRecorder()
		handler(w, r.WithContext(ctx))
		return w.Code
	}

	if code := ask(http.MethodDelete, maya, "EG_1"); code != http.StatusForbidden || deleted["EG_1"] {
		t.Errorf("a guest who may only read the event: %d (deleted: %v), want 403 and nothing deleted", code, deleted["EG_1"])
	}
	if code := ask(http.MethodDelete, lee, "EG_1"); code != http.StatusNotFound || deleted["EG_1"] {
		t.Errorf("someone who isn't on the event: %d (deleted: %v), want 404 and nothing deleted", code, deleted["EG_1"])
	}
	if code := ask(http.MethodDelete, jonas, "EG_2"); code != http.StatusNotFound {
		t.Errorf("a recording filed under another event: %d, want 404", code)
	}
	if code := ask(http.MethodDelete, jonas, "EG_1"); code != http.StatusOK || !deleted["EG_1"] {
		t.Errorf("its organiser: %d (deleted: %v), want 200 and it deleted", code, deleted["EG_1"])
	}
	if code := ask(http.MethodDelete, jonas, "EG_1"); code != http.StatusNotFound {
		t.Errorf("deleting it again: %d, want 404", code)
	}
	if code := ask(http.MethodPost, maya, "EG_1"); code != http.StatusForbidden || !deleted["EG_1"] {
		t.Errorf("a guest who may only read the event brought it back: %d (deleted: %v), want 403", code, deleted["EG_1"])
	}
	if code := ask(http.MethodPost, jonas, "EG_1"); code != http.StatusOK || deleted["EG_1"] {
		t.Errorf("its organiser's Undo: %d (deleted: %v), want 200 and it back", code, deleted["EG_1"])
	}
	if code := ask(http.MethodPost, jonas, "EG_1"); code != http.StatusNotFound {
		t.Errorf("bringing back one that's there: %d, want 404", code)
	}
}
