package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// fakeFeed answers what it's given and keeps what it was asked.
type fakeFeed struct {
	entries []workFeed.Entry
	next    workFeed.Cursor
	err     error
	asked   int
	who     accessBusiness.Subject
	cursor  workFeed.Cursor
	limit   int
}

func (f *fakeFeed) Since(_ context.Context, who accessBusiness.Subject, cursor workFeed.Cursor, limit int) ([]workFeed.Entry, workFeed.Cursor, error) {
	f.asked++
	f.who, f.cursor, f.limit = who, cursor, limit
	return f.entries, f.next, f.err
}

func (f *fakeFeed) Publish(context.Context, []workFeed.Entry) {}

func use(t *testing.T, f *fakeFeed) {
	t.Helper()
	saved := workFeed.Changes
	t.Cleanup(func() { workFeed.Changes = saved })
	workFeed.Changes = f
}

var ada = userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x7", Uuid: "9c4f5e2b-3a7d-4fab-8d84-4e5f60718293", UserName: "ada"}}

func get(target string, user *userModels.UserInfo) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if user != nil {
		req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, *user))
	}
	rec := httptest.NewRecorder()
	Changes(rec, req)
	return rec
}

// The caller's changes after their place, in the feed's order, and the place
// to ask from next; the feed is asked as the caller. None is [], and no limit
// asks for the most there is.
func TestChangesAnswersTheCallersEntriesAndTheNextPlace(t *testing.T) {
	at := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	f := &fakeFeed{next: "12", entries: []workFeed.Entry{{Sequence: 12, Kind: "task.updated", ResourceKind: "task",
		ResourceUUID: "t1", ProjectUUID: "p1", Diff: json.RawMessage(`{"task_status":"done"}`), At: at}}}
	use(t, f)
	rec := get("/work/changes?cursor=9&limit=50", &ada)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var body struct {
		Data struct {
			Entries []workFeed.Entry `json:"entries"`
			Cursor  string           `json:"cursor"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if e := body.Data.Entries; len(e) != 1 || e[0].Sequence != 12 || e[0].ResourceUUID != "t1" ||
		!strings.Contains(string(e[0].Diff), "done") || body.Data.Cursor != "12" {
		t.Errorf("answered %+v", body.Data)
	}
	if f.asked != 1 || f.cursor != "9" || f.limit != 50 || f.who.UserUUID != ada.UserDgraphInfo.Uuid || f.who.UserUID != "0x7" {
		t.Errorf("asked %d times, as %+v from %q for %d", f.asked, f.who, f.cursor, f.limit)
	}

	f.entries = nil
	rec = get("/work/changes?cursor=12", &ada)
	var none struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &none); err != nil || rec.Code != http.StatusOK ||
		string(none.Data["entries"]) != "[]" || f.limit != workFeed.MaxSince {
		t.Errorf("nothing new: %d %s (limit %d), want entries [] and the most there is", rec.Code, rec.Body, f.limit)
	}
}

// A place the log no longer has is 410 (read your lists again); one that
// isn't a place is 400; a feed that can't answer is 503 and never shows its
// error; no one signed in is 401, before the feed is asked.
func TestChangesRefusesWithWhatTheReaderShouldDo(t *testing.T) {
	for _, c := range []struct {
		name   string
		target string
		user   *userModels.UserInfo
		err    error
		want   int
		asks   bool
	}{
		{"a place that's gone", "/work/changes?cursor=3", &ada, workFeed.ErrCursorExpired, http.StatusGone, true},
		{"not a place", "/work/changes?cursor=x", &ada, workFeed.ErrBadCursor, http.StatusBadRequest, true},
		{"the feed can't answer", "/work/changes?cursor=3", &ada, errors.New("pq: relation task_changes gone"), http.StatusServiceUnavailable, true},
		{"no one signed in", "/work/changes", nil, nil, http.StatusUnauthorized, false},
	} {
		f := &fakeFeed{err: c.err, next: "3"}
		use(t, f)
		rec := get(c.target, c.user)
		if rec.Code != c.want || (f.asked == 1) != c.asks {
			t.Errorf("%s: %d (feed asked %d times), want %d", c.name, rec.Code, f.asked, c.want)
		}
		if strings.Contains(rec.Body.String(), "pq:") {
			t.Errorf("%s: the answer shows the error: %s", c.name, rec.Body)
		}
	}
}

// The limit reads as every list's page size does (helpers.SizeParam): one
// that is missing, not a number or below one asks for the most there is
// (workFeed.MaxSince), and one above it for that most. It is never refused.
func TestChangesReadsItsLimitAsEveryListDoes(t *testing.T) {
	for _, c := range []struct {
		query string
		want  int
	}{
		{"", workFeed.MaxSince},
		{"&limit=50", 50},
		{"&limit=x", workFeed.MaxSince},
		{"&limit=0", workFeed.MaxSince},
		{"&limit=-2", workFeed.MaxSince},
		{"&limit=" + strconv.Itoa(workFeed.MaxSince+1), workFeed.MaxSince},
	} {
		f := &fakeFeed{next: "3"}
		use(t, f)
		if rec := get("/work/changes?cursor=3"+c.query, &ada); rec.Code != http.StatusOK || f.asked != 1 || f.limit != c.want {
			t.Errorf("limit %q: %d, the feed asked %d times for %d; want 200, asked once for %d", c.query, rec.Code, f.asked, f.limit, c.want)
		}
	}
}
