package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	ICS "github.com/akashc777/OneCamp/business/ICS"
)

// A create whose answer was lost is sent again (the outbox's retry, or the
// sweep's next pass); Graph makes one event for it, as it does for a create
// that names the same transactionId as one it has made.
func TestARetriedOutlookCreateWhoseAnswerWasLostMakesOneEvent(t *testing.T) {
	var (
		mu     sync.Mutex
		made   = map[string]string{} // transactionId -> the event Graph made for it
		events []string
		posts  int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/me/calendars/c1/events" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"ErrorItemNotFound"}}`)
			return
		}
		var body struct {
			TransactionID string `json:"transactionId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		posts++
		id, seen := made[body.TransactionID]
		if !seen || body.TransactionID == "" {
			id = fmt.Sprintf("e%d", len(events)+1)
			events = append(events, id)
			made[body.TransactionID] = id
		}
		w.Header().Set("Content-Type", "application/json")
		if posts == 1 { // made, but the answer never reaches OneCamp
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"code":"BadGateway"}}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"id":%q,"changeKey":"k1"}`, id)
	}))
	t.Cleanup(srv.Close)
	a := Adapter{Base: srv.URL, Token: func(context.Context, string) (string, error) { return "tok", nil },
		Client: srv.Client(), Now: func() time.Time { return at("2026-11-05T12:00:00Z") },
		Sleep: func(context.Context, time.Duration) error { return nil }}
	remote := cal1
	remote.Link.ID = uuid.New()
	op := calendarSyncBusiness.Op{Kind: "create", Remote: remote, EventUUID: uuid.New(),
		Event: ICS.Event{UID: "oc-1", Summary: "Plan", Start: at("2026-11-10T09:00:00Z"), End: at("2026-11-10T10:00:00Z"), Zone: "UTC"}}
	ref, err := a.Push(context.Background(), op)
	for try := 0; err != nil && try < 3; try++ {
		ref, err = a.Push(context.Background(), op)
	}
	if err != nil || len(events) != 1 || ref.ExternalID != events[0] {
		t.Fatalf("a retried create made %v (ref %+v, err %v); want one event", events, ref, err)
	}
}
