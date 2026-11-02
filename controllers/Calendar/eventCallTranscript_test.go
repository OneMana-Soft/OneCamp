package Calendar

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Only whoever may read an event reads its call's transcript (meet-02, head
// C2), by the calendar's own rule, as they play its recording. The page asked
// for reaches the read (20 lines unless asked, at most 100), and the answer
// is the conversation transcript's shape: the lines, and how many pages they
// fill. Anyone else is told there is no such event; a transcript filed under
// another event is not found; a graph that doesn't answer is "try again".
func TestOnlyWhoMayReadAnEventReadsItsCallsTranscript(t *testing.T) {
	ev, maya, sam := uuid.New(), uuid.New(), uuid.New()
	wasEvents, wasTranscript := business.Events, eventTranscript
	t.Cleanup(func() { business.Events, eventTranscript = wasEvents, wasTranscript })
	business.Events = artefactKeysOnly{keys: func(who *model.UserInfo, id string) (business.CallKeys, error) {
		if id != "e1" || who.UserPostgresInfo.Id != maya {
			return business.CallKeys{}, business.ErrEventNotFound
		}
		return business.CallKeys{Events: []uuid.UUID{ev}}, nil
	}}
	var asked helpers.Page
	eventTranscript = func(_ context.Context, keys business.CallKeys, key string, page helpers.Page) (*dgraphStruct.DgraphRecording, error) {
		asked = page
		if key == "EG_down" {
			return nil, errors.New("the graph didn't answer")
		}
		if key != "EG_1" || !slices.Contains(keys.Events, ev) {
			return nil, nil
		}
		line := &dgraphStruct.DgraphTranscript{Text: "The rollback steps go in the notes."}
		return &dgraphStruct.DgraphRecording{EgressId: key, TranscriptCount: 45, Transcript: []*dgraphStruct.DgraphTranscript{line}}, nil
	}
	type answer struct {
		Data struct {
			Lines []struct {
				Text string `json:"transcript_text"`
			} `json:"recording_transcript"`
			Count uint64 `json:"recording_transcript_count"`
		} `json:"data"`
		PageCount uint64 `json:"pageCount"`
	}
	ask := func(who uuid.UUID, key, query string) (int, answer) {
		r := httptest.NewRequest(http.MethodGet, "/calendar/events/e1/call/artefacts/"+key+"/transcript"+query, nil)
		rc := chi.NewRouteContext()
		rc.URLParams.Add("eventId", "e1")
		rc.URLParams.Add("key", key)
		info := model.UserInfo{}
		info.UserPostgresInfo.Id, info.UserDgraphInfo.Uuid = who, who.String()
		ctx := context.WithValue(context.WithValue(r.Context(), chi.RouteCtxKey, rc), helpers.UserInfoContextKey, info)
		w := httptest.NewRecorder()
		GetCalendarEventCallTranscript(w, r.WithContext(ctx))
		var out answer
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	code, out := ask(maya, "EG_1", "?pageIndex=1&pageSize=20")
	if code != http.StatusOK || len(out.Data.Lines) != 1 || out.Data.Lines[0].Text != "The rollback steps go in the notes." || out.Data.Count != 45 {
		t.Errorf("someone who may read the event: %d %+v, want 200 and the transcript's page", code, out)
	}
	if out.PageCount != 3 {
		t.Errorf("45 lines at 20 a page fill %d pages, want 3", out.PageCount)
	}
	if want := (helpers.Page{Index: 1, Size: 20}); asked != want {
		t.Errorf("the read was asked for %+v, want %+v", asked, want)
	}
	if code, _ := ask(maya, "EG_1", ""); code != http.StatusOK || asked != (helpers.Page{Index: 0, Size: 20}) {
		t.Errorf("no page asked: %d, read %+v; want 200 and the first 20 lines", code, asked)
	}
	if code, _ := ask(maya, "EG_1", "?pageIndex=0&pageSize=5000"); code != http.StatusOK || asked.Size != 100 {
		t.Errorf("5,000 lines asked: %d, read %+v; want at most 100", code, asked)
	}
	if code, _ := ask(sam, "EG_1", ""); code != http.StatusNotFound {
		t.Errorf("someone who may not read the event: %d, want 404", code)
	}
	if code, _ := ask(maya, "EG_2", ""); code != http.StatusNotFound {
		t.Errorf("a transcript filed under another event: %d, want 404", code)
	}
	if code, _ := ask(maya, "EG_down", ""); code != http.StatusServiceUnavailable {
		t.Errorf("a graph that doesn't answer: %d, want 503", code)
	}
}
