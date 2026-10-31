package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	globalSearch "github.com/akashc777/OneCamp/business/GlobalSearch"
	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// searched is what the engine was asked, in a test.
type searched struct {
	calls int
	reach globalSearch.Reach
	spec  querySpec.Spec
	page  globalSearch.Page
}

func searchWith(t *testing.T, result globalSearch.Result, err error) *searched {
	t.Helper()
	savedRun, savedAllow := runSearch, allowSearch
	t.Cleanup(func() { runSearch, allowSearch = savedRun, savedAllow })
	allowSearch = func(context.Context, string) (bool, int) { return true, 0 }
	got := &searched{}
	runSearch = func(_ context.Context, reach globalSearch.Reach, spec querySpec.Spec, page globalSearch.Page) (globalSearch.Result, error) {
		got.calls++
		got.reach, got.spec, got.page = reach, spec, page
		return result, err
	}
	return got
}

func postSearch(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	member := sam
	member.UserDgraphInfo.Channels = []*dgraphStruct.DgraphChannel{{Uuid: "c1"}}
	member.UserDgraphInfo.Projects = []*dgraphStruct.DgraphProject{{Uuid: "p1"}}
	r := httptest.NewRequest(http.MethodPost, "/query/search", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, member))
	rec := httptest.NewRecorder()
	SearchQuery(rec, r)
	return rec
}

// A search answers the page, its counts, its cursor and the query as read,
// as the signed-in person: their reach, their zone, the kind on show.
func TestASearchRunsAsThePersonAndSaysWhatItRead(t *testing.T) {
	hit := &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.TASK_TYPE, Task: &openSearchStruct.OpenSearchTask{Uuid: "t1", TaskName: "Launch"}}
	got := searchWith(t, globalSearch.Result{Page: []*openSearchStruct.GlobalSearchOpenSearchResp{hit}, Totals: map[string]int{"task": 3}, Total: 3, HasMore: true, Next: "abc"}, nil)
	rec := postSearch(t, `{"q": "launch -draft", "kinds": ["task"], "sort": "newest", "size": 10, "zone": "Asia/Kolkata"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var answer struct {
		Data struct {
			Page    []json.RawMessage `json:"page"`
			Totals  map[string]int    `json:"totals"`
			Total   int               `json:"total"`
			HasMore bool              `json:"has_more"`
			Next    string            `json:"next"`
			Query   struct {
				Runs   bool              `json:"runs"`
				Text   string            `json:"text"`
				Tokens []json.RawMessage `json:"tokens"`
				Issues []json.RawMessage `json:"issues"`
			} `json:"query"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	d := answer.Data
	if len(d.Page) != 1 || d.Totals["task"] != 3 || d.Total != 3 || !d.HasMore || d.Next != "abc" {
		t.Errorf("the answer: %+v", d)
	}
	if !d.Query.Runs || d.Query.Text != "launch -draft" || len(d.Query.Tokens) == 0 || d.Query.Issues == nil {
		t.Errorf("the query as read: %+v", d.Query)
	}
	if got.calls != 1 || got.reach.UserUUID != sam.UserDgraphInfo.Uuid || len(got.reach.Channels) != 1 || got.reach.Projects[0] != "p1" {
		t.Errorf("the reach: %+v", got.reach)
	}
	if got.page.Size != 10 || got.page.Sort != "newest" || len(got.page.Kinds) != 1 || got.page.Zone == nil || got.page.Zone.String() != "Asia/Kolkata" {
		t.Errorf("the page asked for: %+v", got.page)
	}
	if got.spec.Filter == nil || got.spec.Filter.Op != querySpec.OpAnd {
		t.Errorf("the spec: %+v", got.spec.Filter)
	}
}

// One conversation narrows the search; a DM is the asker's own with that
// person.
func TestASearchFromAConversationStaysInIt(t *testing.T) {
	for body, want := range map[string]querySpec.Value{
		`{"q": "launch", "channel": "c9"}`:   {ID: "c9", Type: "channel"},
		`{"q": "launch", "group": "g1 g2"}`:  {ID: "g1 g2", Type: "conversation"},
		`{"q": "launch", "dm": "ana-uuid"}`:  {ID: helpers.GetGroupingId(sam.UserDgraphInfo.Uuid, "ana-uuid"), Type: "conversation"},
		`{"q": "launch", "channel": " c9 "}`: {ID: "c9", Type: "channel"},
	} {
		got := searchWith(t, globalSearch.Result{}, nil)
		if rec := postSearch(t, body); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body)
		}
		f := got.spec.Filter
		if f == nil || f.Op != querySpec.OpAnd || len(f.Filters) != 2 {
			t.Fatalf("%s: %+v", body, f)
		}
		in := f.Filters[1]
		if in.Field != "in" || len(in.Values) != 1 || in.Values[0].ID != want.ID || in.Values[0].Type != want.Type {
			t.Errorf("%s: narrowed by %+v, want %+v", body, in, want)
		}
	}
	got := searchWith(t, globalSearch.Result{}, nil)
	postSearch(t, `{"q": "launch"}`)
	if got.spec.Filter == nil || got.spec.Filter.Field != "text" {
		t.Errorf("no conversation: %+v", got.spec.Filter)
	}
}

// What the endpoint can't take is a 400 that says why; a text past a limit
// is a 200 with its issue and no search; nothing typed searches nothing.
func TestASearchRefusesWhatItCantTake(t *testing.T) {
	for body, status := range map[string]int{
		`{"q": "launch", "kinds": ["everything"]}`:             http.StatusBadRequest,
		`{"q": "launch", "sort": "random"}`:                    http.StatusBadRequest,
		`{"q": "launch", "size": 51}`:                          http.StatusBadRequest,
		`{"q": "launch", "size": -1}`:                          http.StatusBadRequest,
		`not json`:                                             http.StatusBadRequest,
		`{"q": "   "}`:                                         http.StatusOK,
		`{"q": "` + strings.Repeat("a ", 26) + `(((((x)))))"}`: http.StatusOK,
	} {
		got := searchWith(t, globalSearch.Result{}, nil)
		rec := postSearch(t, body)
		if rec.Code != status {
			t.Errorf("%.40s: %d, want %d (%s)", body, rec.Code, status, rec.Body)
		}
		if got.calls != 0 {
			t.Errorf("%.40s searched", body)
		}
		if status == http.StatusBadRequest && !strings.Contains(rec.Body.String(), `"msg"`) {
			t.Errorf("%.40s: no message: %s", body, rec.Body)
		}
	}
}

// A page the server didn't write is a 400; an engine that fails is a retry,
// never results that aren't there; past the rate, 429 with when to come back.
func TestASearchSaysWhenItCantAnswer(t *testing.T) {
	searchWith(t, globalSearch.Result{}, globalSearch.ErrBadSearchCursor)
	if rec := postSearch(t, `{"q": "launch", "after": "nope"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("a bad cursor: %d", rec.Code)
	}
	searchWith(t, globalSearch.Result{}, errors.New("index down"))
	if rec := postSearch(t, `{"q": "launch"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("the engine failing: %d %s", rec.Code, rec.Body)
	}
	got := searchWith(t, globalSearch.Result{}, nil)
	allowSearch = func(context.Context, string) (bool, int) { return false, 12 }
	rec := postSearch(t, `{"q": "launch"}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "12" || got.calls != 0 {
		t.Errorf("past the rate: %d, Retry-After %q, %d searches", rec.Code, rec.Header().Get("Retry-After"), got.calls)
	}
}

func TestAZoneIsTheAskersOrUTC(t *testing.T) {
	for name, want := range map[string]string{"Europe/London": "Europe/London", "": "UTC", "Mars/Olympus": "UTC", strings.Repeat("x", 80): "UTC"} {
		if got := zoneOf(name); got.String() != want {
			t.Errorf("%q: %s, want %s", name, got, want)
		}
	}
}
