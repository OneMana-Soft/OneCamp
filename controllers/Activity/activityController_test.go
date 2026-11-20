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

	business "github.com/akashc777/OneCamp/business/Activity"
	"github.com/akashc777/OneCamp/helpers"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The feed reads at most a page, whatever the app asks for. Its limit was the
// client's, uncapped, and it drives the notices too (limit+1 of them), each a
// Postgres read, an access check and a title read: ?limit=1000000 asked for a
// million. A limit that isn't a page size (none, junk, below one) is the
// default; one below zero also cut the feed at [:-5], which panics.
func TestTheFeedReadsAtMostAPage(t *testing.T) {
	prevFeed := unifiedActivity
	t.Cleanup(func() { unifiedActivity = prevFeed })
	var asked business.FeedQuery
	unifiedActivity = func(_ context.Context, q business.FeedQuery) (dgraphModels.UnifiedActivityPagination, error) {
		asked = q
		return dgraphModels.UnifiedActivityPagination{}, nil
	}
	for _, c := range []struct {
		limit string
		want  int
	}{
		{"1000000", maxActivityPage},
		{"51", maxActivityPage},
		{"50", 50},
		{"10", 10},
		{"", activityPageSize},
		{"twenty", activityPageSize},
		{"0", activityPageSize},
		{"-5", activityPageSize},
	} {
		asked = business.FeedQuery{}
		req := httptest.NewRequest(http.MethodGet, "/activity/unified?notices=1&limit="+c.limit, nil).
			WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, userModels.UserInfo{}))
		rec := httptest.NewRecorder()
		GetUnifiedActivity(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("limit %q answered %d %s", c.limit, rec.Code, rec.Body.String())
		}
		// The page's limit drives the notices too (limit+1 of them).
		if asked.Limit != c.want || !asked.Notices {
			t.Errorf("limit %q asked for %+v; want a page of %d with the notices", c.limit, asked, c.want)
		}
	}
}

// The badge's two answers are the server's count or nothing: a count that
// can't be read is 503 (the app keeps its last number), never a 0 the server
// didn't count; opening the Inbox answers the count after the write, and none
// when it couldn't be counted (the app asks again).
func TestTheBadgeIsAnsweredOnlyWithACount(t *testing.T) {
	prevSeen, prevCount := inboxSeen, unreadCount
	t.Cleanup(func() { inboxSeen, unreadCount = prevSeen, prevCount })
	ask := func(method, path string, h http.HandlerFunc) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil).
			WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, userModels.UserInfo{}))
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}
	// The count the answer carries: nil when it carries none.
	countIn := func(rec *httptest.ResponseRecorder) any {
		var body struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body.Data["count"]
	}
	var countErr error
	unreadCount = func(context.Context, uuid.UUID, string) (uint64, error) { return 7, countErr }
	if rec := ask(http.MethodGet, "/activity/unread", GetActivityUnread); rec.Code != http.StatusOK || countIn(rec) != float64(7) {
		t.Errorf("GET /activity/unread answered %d %s; want 200 with the count, 7", rec.Code, rec.Body.String())
	}
	countErr = errors.New("dgraph is down")
	if rec := ask(http.MethodGet, "/activity/unread", GetActivityUnread); rec.Code != http.StatusServiceUnavailable || countIn(rec) != nil {
		t.Errorf("a count that can't be read answered %d %s; want 503 and no count", rec.Code, rec.Body.String())
	}

	zero := uint64(0)
	var seen *uint64
	var seenErr error
	inboxSeen = func(context.Context, *userModels.UserInfo) (*uint64, error) { return seen, seenErr }
	seen = &zero
	if rec := ask(http.MethodPost, "/activity/seen", MarkActivitySeen); rec.Code != http.StatusOK || countIn(rec) != float64(0) {
		t.Errorf("POST /activity/seen answered %d %s; want 200 with the count after the write, 0", rec.Code, rec.Body.String())
	}
	seen = nil
	if rec := ask(http.MethodPost, "/activity/seen", MarkActivitySeen); rec.Code != http.StatusOK || countIn(rec) != nil {
		t.Errorf("a seen whose count couldn't be read answered %d %s; want 200 and no count", rec.Code, rec.Body.String())
	}
	seenErr = errors.New("postgres is down")
	if rec := ask(http.MethodPost, "/activity/seen", MarkActivitySeen); rec.Code != http.StatusServiceUnavailable || countIn(rec) != nil {
		t.Errorf("a seen that couldn't be written answered %d %s; want 503 and no count", rec.Code, rec.Body.String())
	}
}

// POST /activity/dismiss takes {keys, dismissed} from the signed-in person:
// a body that isn't one, or a key that isn't a Priority item's (business
// Dismiss refuses it before any write), is 400; the write is the session
// person's, whatever user id the body carries; one that fails is 503.
func TestDismissRefusesABadKey(t *testing.T) {
	prev := dismissFromPriority
	t.Cleanup(func() { dismissFromPriority = prev })
	me := uuid.New()
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/activity/dismiss", strings.NewReader(body)).
			WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: userModels.User{Id: me}}))
		rec := httptest.NewRecorder()
		DismissFromPriority(rec, req)
		return rec
	}
	many := make([]string, 21)
	for i := range many {
		many[i] = `"MENTION|id|0x` + strconv.Itoa(i) + `"`
	}
	dismissFromPriority = business.Dismiss // refuses before it writes
	for _, body := range []string{
		``,
		`not json`,
		`{"keys":["MENTION|id|0x1"]}`,
		`{"dismissed":true}`,
		`{"keys":[],"dismissed":true}`,
		`{"keys":["REACTION|id|0x1"],"dismissed":true}`,
		`{"keys":[""],"dismissed":false}`,
		`{"keys":["MENTION|id|0x1","NOTICE"],"dismissed":true}`,
		`{"keys":[` + strings.Join(many, ",") + `],"dismissed":true}`,
	} {
		if rec := post(body); rec.Code != http.StatusBadRequest {
			t.Errorf("POST /activity/dismiss %s answered %d %s; want 400", body, rec.Code, rec.Body.String())
		}
	}

	var wrote struct {
		user      uuid.UUID
		keys      []string
		dismissed bool
	}
	var writeErr error
	dismissFromPriority = func(_ context.Context, user uuid.UUID, keys []string, dismissed bool) error {
		wrote.user, wrote.keys, wrote.dismissed = user, keys, dismissed
		return writeErr
	}
	someoneElse := uuid.NewString()
	if rec := post(`{"keys":["MENTION|id|0x1"],"dismissed":true,"user_id":"` + someoneElse + `"}`); rec.Code != http.StatusNoContent {
		t.Errorf("a dismissal answered %d %s; want 204", rec.Code, rec.Body.String())
	}
	if wrote.user != me || len(wrote.keys) != 1 || wrote.keys[0] != "MENTION|id|0x1" || !wrote.dismissed {
		t.Errorf("wrote %+v; want the session person's dismissal of MENTION|id|0x1", wrote)
	}
	writeErr = errors.New("postgres is down")
	if rec := post(`{"keys":["MENTION|id|0x1"],"dismissed":false}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a write that failed answered %d %s; want 503", rec.Code, rec.Body.String())
	}
}
