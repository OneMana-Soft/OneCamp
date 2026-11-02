package Calendar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// artefactKeysOnly is the calendar answering only who may read an event's
// call's artefacts.
type artefactKeysOnly struct {
	business.Service
	keys func(who *model.UserInfo, id string) (business.CallKeys, error)
}

func (f artefactKeysOnly) CallArtefactKeys(_ context.Context, who *model.UserInfo, id string) (business.CallKeys, error) {
	return f.keys(who, id)
}

// Only whoever may read an event plays its call's recordings (head D), by the
// calendar's own rule, as they read its "Recordings & notes": anyone else is
// told there is no such event, and a recording filed under another event is
// not found.
func TestOnlyWhoMayReadAnEventPlaysItsCallsRecordings(t *testing.T) {
	ev, maya, sam := uuid.New(), uuid.New(), uuid.New()
	wasEvents, wasRec := business.Events, eventRecording
	t.Cleanup(func() { business.Events, eventRecording = wasEvents, wasRec })
	business.Events = artefactKeysOnly{keys: func(who *model.UserInfo, id string) (business.CallKeys, error) {
		if id != "e1" || who.UserPostgresInfo.Id != maya {
			return business.CallKeys{}, business.ErrEventNotFound
		}
		return business.CallKeys{Events: []uuid.UUID{ev}}, nil
	}}
	eventRecording = func(_ context.Context, keys business.CallKeys, key string) (*dgraphStruct.DgraphRecording, error) {
		if key == "EG_1" && slices.Contains(keys.Events, ev) {
			notEnded := time.Time{}
			return &dgraphStruct.DgraphRecording{EgressId: key, EndedAt: &notEnded}, nil
		}
		return nil, nil
	}
	ask := func(who uuid.UUID, key string) (int, string) {
		r := httptest.NewRequest(http.MethodGet, "/calendar/events/e1/call/artefacts/"+key+"/recording", nil)
		rc := chi.NewRouteContext()
		rc.URLParams.Add("eventId", "e1")
		rc.URLParams.Add("key", key)
		info := model.UserInfo{}
		info.UserPostgresInfo.Id, info.UserDgraphInfo.Uuid = who, who.String()
		ctx := context.WithValue(context.WithValue(r.Context(), chi.RouteCtxKey, rc), helpers.UserInfoContextKey, info)
		w := httptest.NewRecorder()
		GetCalendarEventCallRecording(w, r.WithContext(ctx))
		var out struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Status
	}
	if code, status := ask(maya, "EG_1"); code != http.StatusOK || status != "recording" {
		t.Errorf("someone who may read the event: %d %q, want 200 and the recording's status", code, status)
	}
	if code, _ := ask(sam, "EG_1"); code != http.StatusNotFound {
		t.Errorf("someone who may not read the event: %d, want 404", code)
	}
	if code, _ := ask(maya, "EG_2"); code != http.StatusNotFound {
		t.Errorf("a recording filed under another event: %d, want 404", code)
	}
}
