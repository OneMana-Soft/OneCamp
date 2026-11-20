package Integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	ICS "github.com/akashc777/OneCamp/business/ICS"
)

// A create whose answer was lost is sent again (the outbox's retry, or the
// sweep's next pass). Google keys an imported event by its iCalUID and says
// 409 to a second one; that answer is the event made already, read back, so
// the retry makes no second event and its ref is kept.
func TestARetriedGoogleCreateWhoseAnswerWasLostMakesOneEvent(t *testing.T) {
	var (
		mu     sync.Mutex
		made   = map[string]string{} // iCalUID -> the event Google made for it
		marks  = map[string]string{} // the event -> the OneCamp uuid it is marked with
		events []string
		posts  int
	)
	withSyncService(t, fakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events/import"):
			body, _ := io.ReadAll(r.Body)
			var ev struct {
				ICalUID            string `json:"iCalUID"`
				ExtendedProperties struct {
					Private map[string]string `json:"private"`
				} `json:"extendedProperties"`
			}
			_ = json.Unmarshal(body, &ev)
			posts++
			if _, seen := made[ev.ICalUID]; seen {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":{"code":409,"message":"The requested identifier already exists.","errors":[{"reason":"duplicate"}]}}`)
				return
			}
			id := fmt.Sprintf("g%d", len(events)+1)
			events = append(events, id)
			made[ev.ICalUID], marks[id] = id, ev.ExtendedProperties.Private[googleEventKey]
			if posts == 1 { // made, but the answer never reaches OneCamp
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"error":{"code":502}}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"id":%q,"etag":"\"1\""}`, id)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			var items []string
			if id, ok := made[r.URL.Query().Get("iCalUID")]; ok {
				items = append(items, fmt.Sprintf(`{"id":%q,"etag":"\"2\"","iCalUID":%q,"extendedProperties":{"private":{%q:%q}}}`, id, r.URL.Query().Get("iCalUID"), googleEventKey, marks[id]))
			}
			_, _ = io.WriteString(w, `{"items":[`+strings.Join(items, ",")+`]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404}}`)
		}
	}))
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	op := calendarSyncBusiness.Op{Kind: "create", EventUUID: uuid.New(),
		Remote: calendarSyncBusiness.Remote{Link: calendarSyncBusiness.Link{ID: uuid.New(), PersonUUID: "p"}, ID: "primary"},
		Event:  ICS.Event{UID: "abc@onecamp", Summary: "Plan", Start: start, End: start.Add(time.Hour), Zone: "UTC"}}
	ref, err := googleAdapter{}.Push(context.Background(), op)
	for try := 0; err != nil && try < 3; try++ {
		ref, err = googleAdapter{}.Push(context.Background(), op)
	}
	if err != nil || len(events) != 1 || ref.ExternalID != events[0] {
		t.Fatalf("a retried create made %v (ref %+v, err %v); want one event", events, ref, err)
	}
}
