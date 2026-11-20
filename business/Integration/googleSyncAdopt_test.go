package Integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	ICS "github.com/akashc777/OneCamp/business/ICS"
)

// CR-383 F2: a create Google answers 409 for (an event of that iCalUID is
// there already) takes only the event OneCamp marked as this one's, live.
// iCalUIDs aren't one person's: Bea's copy of Sam's event is sent under
// Sam's UID, so on a calendar both map, Sam's event holds it. Another
// event's, or this one's cancelled, is never taken, and no second event is
// made: the change is refused (kept, waiting until the event changes).
func TestAGoogleCreateAdoptsOnlyItsOwnMarkedLiveEvent(t *testing.T) {
	ours := uuid.New()
	marked := func(id, status string, who uuid.UUID) string {
		return fmt.Sprintf(`{"id":%q,"etag":"\"2\"","iCalUID":"abc@onecamp","status":%q,"extendedProperties":{"private":{%q:%q}}}`, id, status, googleEventKey, who.String())
	}
	for _, c := range []struct {
		name  string
		items []string
		want  string // the event taken, "" for a refusal
	}{
		{"only another event's", []string{`{"id":"a1","etag":"\"2\"","iCalUID":"abc@onecamp","status":"confirmed"}`, marked("a2", "confirmed", uuid.New())}, ""},
		{"its own, cancelled", []string{marked("c1", "cancelled", ours)}, ""},
		{"its own, live", []string{marked("a1", "confirmed", uuid.New()), marked("m1", "confirmed", ours)}, "m1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var sent string // the marker the import carried
			withSyncService(t, fakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events/import"):
					body, _ := io.ReadAll(r.Body)
					var ev struct {
						ExtendedProperties struct {
							Private map[string]string `json:"private"`
						} `json:"extendedProperties"`
					}
					_ = json.Unmarshal(body, &ev)
					sent = ev.ExtendedProperties.Private[googleEventKey]
					w.WriteHeader(http.StatusConflict)
					_, _ = io.WriteString(w, `{"error":{"code":409,"message":"The requested identifier already exists.","errors":[{"reason":"duplicate"}]}}`)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
					_, _ = io.WriteString(w, `{"items":[`+strings.Join(c.items, ",")+`]}`)
				default:
					w.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(w, `{"error":{"code":404}}`)
				}
			}))
			start := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
			op := calendarSyncBusiness.Op{Kind: "create", EventUUID: ours,
				Remote: calendarSyncBusiness.Remote{Link: calendarSyncBusiness.Link{ID: uuid.New(), PersonUUID: "p"}, ID: "shared"},
				Event:  ICS.Event{UID: "abc@onecamp", Summary: "Plan", Start: start, End: start.Add(time.Hour), Zone: "UTC"}}
			ref, err := googleAdapter{}.Push(context.Background(), op)
			if sent != ours.String() {
				t.Errorf("the import carried marker %q, want %q", sent, ours)
			}
			if c.want == "" {
				if err == nil || !errors.Is(err, calendarSyncBusiness.ErrRefused) || ref.ExternalID != "" {
					t.Fatalf("took %+v (err %v); want nothing taken, the change refused", ref, err)
				}
				return
			}
			if err != nil || ref.ExternalID != c.want {
				t.Fatalf("took %+v (err %v); want %s", ref, err, c.want)
			}
		})
	}
}
