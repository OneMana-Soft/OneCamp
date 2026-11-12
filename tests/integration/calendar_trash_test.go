//go:build integration

package integration_test

// The calendar's trash pages over HTTP (calendar V-603), against real
// Postgres 12: what is deleted at once (a series with its exceptions, a
// batch) shares one deleted_at, so a page of 50 can end inside such a group.
// The next page goes on from the last event the page gave, its time and its
// id (an instance's "<series>_<start>" too), and holds the rest of the group,
// every event once. An older client that sends the time alone gets what it
// always did; an id that is no event's is refused in words.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestTheTrashPagesThroughEventsDeletedAtOnceOverHTTP$' -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
)

func TestTheTrashPagesThroughEventsDeletedAtOnceOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	ctx := context.Background()
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	at := func(d time.Duration) string { return day.Add(d).Format(time.RFC3339) }
	made := func(title, extra string) calendarBusiness.Event {
		t.Helper()
		code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
			`{"title":"`+title+`","start":{"at":"`+at(9*time.Hour)+`","zone":"UTC"},"end":{"at":"`+at(10*time.Hour)+`"}`+extra+`}`, "")
		if code != http.StatusCreated {
			t.Fatalf("%s: %d %v", title, code, body)
		}
		return dataEvent(t, body)
	}

	// 49 events and two instances of a series: 51 in the trash.
	var ids []string
	for i := 0; i < 49; i++ {
		e := made(fmt.Sprintf("Event %d", i), "")
		if err := calendarBusiness.Events.Delete(ctx, sam, e.ID, "", nil); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID)
	}
	series := made("Stand-up", `,"recurrence":{"rule":"FREQ=WEEKLY"}`)
	for _, week := range []int{1, 2} {
		ref := series.ID + "_" + day.Add(9*time.Hour).AddDate(0, 0, 7*week).Format("20060102T150405Z")
		if err := calendarBusiness.Events.Delete(ctx, sam, ref, calendarBusiness.ScopeThis, nil); err != nil {
			t.Fatalf("delete %s: %v", ref, err)
		}
		ids = append(ids, ref)
	}
	// All deleted at one moment, as in one transaction.
	res, err := env.PG.Exec(`UPDATE events SET deleted_at = date_trunc('second', NOW()) WHERE created_by = $1 AND deleted_at IS NOT NULL`, sam.UserDgraphInfo.Uuid)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 51 {
		t.Fatalf("%d rows in the trash, want 51", n)
	}

	read := func(query string) (int, []calendarBusiness.Event, map[string]json.RawMessage) {
		t.Helper()
		code, body := v2(t, eventController.CalendarTrash, http.MethodGet, "/calendar/events/trash"+query, sam, "", "")
		var page []calendarBusiness.Event
		if code == http.StatusOK {
			if err := json.Unmarshal(body["data"], &page); err != nil {
				t.Fatalf("%s: %v %v", query, err, body)
			}
		}
		return code, page, body
	}
	seen := map[string]int{}
	query := ""
	var last calendarBusiness.Event
	for pages := 0; ; pages++ {
		code, page, body := read(query)
		if code != http.StatusOK || pages > 2 {
			t.Fatalf("page %d: %d %v", pages+1, code, body)
		}
		for _, e := range page {
			seen[e.ID]++
		}
		if len(page) < 50 {
			break
		}
		last = page[len(page)-1]
		query = "?before=" + url.QueryEscape(last.DeletedAt.Format(time.RFC3339Nano)) + "&before_id=" + url.QueryEscape(last.ID)
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Errorf("%s shows %d times in the trash, want once", id, seen[id])
		}
	}
	if len(seen) != len(ids) {
		t.Errorf("the trash shows %d events, want %d", len(seen), len(ids))
	}

	// An older client sends the time alone: nothing was deleted before it.
	before := "?before=" + url.QueryEscape(last.DeletedAt.Format(time.RFC3339Nano))
	if code, page, body := read(before); code != http.StatusOK || len(page) != 0 {
		t.Errorf("from the time alone: %d, %d events %v; want none", code, len(page), body)
	}
	// An id that is no event's is refused in words.
	if code, _, body := read(before + "&before_id=not-an-id"); code != http.StatusBadRequest || len(body["msg"]) == 0 {
		t.Errorf("from a bad id: %d %v, want 400 in words", code, body)
	}
}
