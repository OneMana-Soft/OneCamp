package monday

// Transport and crawl tests against an httptest GraphQL server, so no
// monday.com account is needed in CI.
//
// What this catches:
//   - Authorization / API-Version header drift
//   - every limit shape monday uses (429 + extensions, 200 + errors,
//     legacy error_code) being absorbed and retried, and the daily cap
//     surfacing as ErrRateLimited instead of a silent hang
//   - a bad token failing fast with an actionable message
//   - items_page → next_items_page cursor pagination, users paging,
//     board filtering (docs, archived, empty), subitems, updates paging

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

type gqlReq struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// withFakeMonday points the transport at a test server and stubs the
// retry sleep, recording each wait.
func withFakeMonday(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, req gqlReq)) *[]time.Duration {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req gqlReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		handler(w, r, req)
	}))
	prevEndpoint, prevSleep := graphqlEndpoint, sleepFn
	var mu sync.Mutex
	waits := []time.Duration{}
	graphqlEndpoint = srv.URL
	sleepFn = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() {
		graphqlEndpoint, sleepFn = prevEndpoint, prevSleep
		srv.Close()
	})
	return &waits
}

func TestValidate_SendsRawTokenAndPinnedVersion(t *testing.T) {
	withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		if got := r.Header.Get("Authorization"); got != "tok-123" {
			t.Errorf("Authorization = %q, want the raw token", got)
		}
		if got := r.Header.Get("API-Version"); got != apiVersion {
			t.Errorf("API-Version = %q", got)
		}
		if !strings.Contains(req.Query, "me") {
			t.Errorf("validate should query me, got %q", req.Query)
		}
		fmt.Fprint(w, `{"data":{"me":{"id":"4012","name":"Ada","email":"ada@acme.test"}},"account_id":1}`)
	})
	// A pasted "Bearer " prefix is tolerated.
	if err := New().validateToken(context.Background(), "Bearer tok-123"); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidate_BadTokenFailsFastWithHowToFix(t *testing.T) {
	var hits int32
	withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"errors":[{"message":"Not Authenticated","extensions":{"code":"Unauthorized"}}]}`)
	})
	err := New().validateToken(context.Background(), "bad")
	if err == nil || !strings.Contains(err.Error(), "rejected the API token") || !strings.Contains(err.Error(), "My access tokens") {
		t.Fatalf("want actionable auth error, got %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("auth failure must not be retried, got %d hits", hits)
	}
}

func TestValidate_AuthErrorOn200Envelope(t *testing.T) {
	withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		fmt.Fprint(w, `{"errors":[{"message":"Not Authenticated","extensions":{"code":"NOT_AUTHENTICATED"}}],"data":null}`)
	})
	if err := New().validateToken(context.Background(), "bad"); err == nil || !strings.Contains(err.Error(), "rejected the API token") {
		t.Fatalf("got %v", err)
	}
}

func TestGQL_Complexity429ThenSuccess(t *testing.T) {
	var hits int32
	waits := withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"errors":[{"message":"Complexity budget exhausted","extensions":{"code":"COMPLEXITY_BUDGET_EXHAUSTED","retry_in_seconds":17}}]}`)
			return
		}
		fmt.Fprint(w, `{"data":{"ok":true}}`)
	})
	var out struct {
		OK bool `json:"ok"`
	}
	if err := New().gql(context.Background(), "tok", `query { ok }`, nil, &out); err != nil {
		t.Fatalf("gql: %v", err)
	}
	if !out.OK || atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("expected a retry then success; ok=%v hits=%d", out.OK, hits)
	}
	if len(*waits) != 1 || (*waits)[0] != 17*time.Second {
		t.Fatalf("should wait retry_in_seconds=17s, waited %v", *waits)
	}
}

func TestGQL_LegacyComplexityShapeOn200ThenSuccess(t *testing.T) {
	var hits int32
	waits := withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		if atomic.AddInt32(&hits, 1) == 1 {
			fmt.Fprint(w, `{"error_code":"ComplexityException","status_code":429,"error_message":"Complexity budget exhausted, query cost 30001 budget remaining 6017 out of 1000000 reset in 13 seconds","error_data":{},"account_id":1}`)
			return
		}
		fmt.Fprint(w, `{"data":{"ok":true}}`)
	})
	var out struct {
		OK bool `json:"ok"`
	}
	if err := New().gql(context.Background(), "tok", `query { ok }`, nil, &out); err != nil || !out.OK {
		t.Fatalf("gql: %v ok=%v", err, out.OK)
	}
	if len(*waits) != 1 || (*waits)[0] != 13*time.Second {
		t.Fatalf("should parse 'reset in 13 seconds', waited %v", *waits)
	}
}

func TestGQL_Plain429UsesRetryAfter(t *testing.T) {
	var hits int32
	waits := withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "4")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"data":{"ok":true}}`)
	})
	var out struct{ OK bool }
	if err := New().gql(context.Background(), "tok", `query { ok }`, nil, &out); err != nil {
		t.Fatalf("gql: %v", err)
	}
	if len(*waits) != 1 || (*waits)[0] != 4*time.Second {
		t.Fatalf("waited %v, want 4s from Retry-After", *waits)
	}
}

func TestGQL_DailyLimitSurfacesAsRateLimited(t *testing.T) {
	var hits int32
	withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"errors":[{"message":"Daily limit exceeded","extensions":{"code":"DAILY_LIMIT_EXCEEDED","retry_in_seconds":3600}}]}`)
	})
	var out struct{}
	err := New().gql(context.Background(), "tok", `query { ok }`, nil, &out)
	d, ok := importProvider.IsRateLimited(err)
	if !ok || d != time.Hour {
		t.Fatalf("want ErrRateLimited(1h), got %v (%v)", err, d)
	}
	if !strings.Contains(err.Error(), "daily") {
		t.Fatalf("reason should name the daily cap: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("an hour-long wait must not be slept inline; hits=%d", hits)
	}
}

func TestGQL_PersistentRateLimitGivesUp(t *testing.T) {
	var hits int32
	withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, `{"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATE_LIMIT_EXCEEDED","retry_in_seconds":2}}]}`)
	})
	var out struct{}
	err := New().gql(context.Background(), "tok", `query { ok }`, nil, &out)
	if _, ok := importProvider.IsRateLimited(err); !ok {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != maxRateRetries+1 {
		t.Fatalf("hits = %d, want %d", got, maxRateRetries+1)
	}
}

func TestGQL_5xxRetriedThenSurfaced(t *testing.T) {
	var hits int32
	withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "upstream down")
	})
	var out struct{}
	err := New().gql(context.Background(), "tok", `query { ok }`, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("want a 502 error, got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != maxServerRetries+1 {
		t.Fatalf("hits = %d", got)
	}
}

func TestGQL_GraphQLErrorWithoutData(t *testing.T) {
	withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		fmt.Fprint(w, `{"errors":[{"message":"Field 'nope' doesn't exist on type 'Query'","extensions":{"code":"undefinedField"}}]}`)
	})
	var out struct{}
	err := New().gql(context.Background(), "tok", `query { nope }`, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "doesn't exist") {
		t.Fatalf("got %v", err)
	}
}

func TestGQL_PartialDataKept(t *testing.T) {
	withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		fmt.Fprint(w, `{"data":{"ok":true},"errors":[{"message":"Photo unavailable.","extensions":{"code":"ASSET_UNAVAILABLE"}}]}`)
	})
	var out struct{ OK bool }
	if err := New().gql(context.Background(), "tok", `query { ok }`, nil, &out); err != nil || !out.OK {
		t.Fatalf("partial data should be kept: %v %v", err, out.OK)
	}
}

func TestGQL_CancelledContextSendsNothing(t *testing.T) {
	withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		t.Errorf("server hit with a cancelled context")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out struct{}
	if err := New().gql(ctx, "tok", `query { ok }`, nil, &out); err == nil {
		t.Fatal("expected context error")
	}
}

// ─── Crawl ──────────────────────────────────────────────────────────

func colJSON(id, typ, title, text, value string) string {
	v := "null"
	if value != "" {
		b, _ := json.Marshal(value)
		v = string(b)
	}
	tx := "null"
	if text != "" {
		b, _ := json.Marshal(text)
		tx = string(b)
	}
	return fmt.Sprintf(`{"id":%q,"type":%q,"text":%s,"value":%s,"column":{"title":%q}}`, id, typ, tx, v, title)
}

func manyUpdates(prefix string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`{"id":"%s%d","body":"<p>u%d</p>","text_body":"u%d","created_at":"2026-09-01T10:%02d:00Z","creator_id":"1","assets":[],"replies":[]}`,
			prefix, i, i, i, i%60)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// fakeAccount serves a small but complete account:
//   - 101 users (two pages of users)
//   - boards: Roadmap (items, two pages), Empty, a doc, an archived board
//   - Roadmap item 11 has 2 subitems, a file, an update with a file and
//     a reply; item 12 has 100 updates so its updates page further
//   - the second items page answers 429 once, then succeeds
//   - the workspace members query fails (exercises the warning path)
func fakeAccount(t *testing.T) (*int32, *[]time.Duration) {
	var nextHits int32
	waits := withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		q := req.Query
		switch {
		case strings.Contains(q, "users("):
			page := int(req.Variables["page"].(float64))
			n := 100
			if page == 2 {
				n = 1
			}
			if page > 2 {
				n = 0
			}
			us := make([]string, n)
			for i := range us {
				id := (page-1)*100 + i + 1
				us[i] = fmt.Sprintf(`{"id":"%d","name":"User %d","email":"u%d@acme.test","photo_thumb":"https://cdn.monday.test/%d.png","is_guest":%v,"enabled":true}`,
					id, id, id, id, id == 2)
			}
			fmt.Fprintf(w, `{"data":{"users":[%s]}}`, strings.Join(us, ","))

		case strings.Contains(q, "boards(limit"):
			if strings.Contains(q, "workspace_ids") {
				t.Errorf("no workspace scope given; query must not filter: %s", q)
			}
			fmt.Fprint(w, `{"data":{"boards":[
			  {"id":"1","name":"Roadmap","description":"Q4 plan","state":"active","board_kind":"public","type":"board","url":"https://acme.monday.com/boards/1",
			   "workspace_id":"55","workspace":{"id":"55","name":"Product","description":"Product team"},
			   "owners":[{"id":"1"}],"subscribers":[{"id":"1"},{"id":"2"}],"creator":{"id":"1"}},
			  {"id":"2","name":"Empty","state":"active","board_kind":"private","type":"board","url":"https://acme.monday.com/boards/2",
			   "workspace_id":null,"workspace":null,"owners":[{"id":"3"}],"subscribers":[],"creator":{"id":"3"}},
			  {"id":"3","name":"Specs","state":"active","board_kind":"public","type":"document","workspace_id":"55","owners":[],"subscribers":[]},
			  {"id":"4","name":"Old","state":"archived","board_kind":"public","type":"board","workspace_id":"55","owners":[],"subscribers":[]}
			]}}`)

		case strings.Contains(q, "items_page(") && !strings.Contains(q, "next_items_page("):
			ids := req.Variables["ids"].([]any)
			if ids[0] == "2" {
				fmt.Fprint(w, `{"data":{"boards":[{"items_page":{"cursor":null,"items":[]}}]}}`)
				return
			}
			item11 := fmt.Sprintf(`{"id":"11","name":"Importer","state":"active","url":"https://acme.monday.com/boards/1/pulses/11",
			  "created_at":"2026-09-01T09:00:00Z","updated_at":"2026-09-05T09:00:00Z","creator_id":"1",
			  "group":{"id":"topics","title":"This week"},
			  "column_values":[%s,%s,%s,%s,%s],
			  "assets":[{"id":"a1","name":"spec.pdf","public_url":"https://files.monday.test/a1?sig=x","file_extension":".pdf","file_size":2048},
			            {"id":"a2","name":"shot.png","public_url":"https://files.monday.test/a2","file_extension":".png","file_size":10}],
			  "updates":[{"id":"u1","body":"<p>Kickoff</p>","text_body":"Kickoff","created_at":"2026-09-02T09:00:00Z","creator_id":"2",
			     "assets":[{"id":"a2","name":"shot.png","public_url":"https://files.monday.test/a2","file_extension":".png","file_size":10}],
			     "replies":[{"id":"r1","body":"<p>On it</p>","created_at":"2026-09-02T10:00:00Z","creator_id":"1"}]}],
			  "subitems":[
			    {"id":"111","name":"Parse columns","state":"active","created_at":"2026-09-01T09:00:00Z","creator_id":"1","group":{"id":"g","title":"Subitems"},
			     "column_values":[%s,%s]},
			    {"id":"112","name":"Write tests","state":"active","created_at":"2026-09-01T09:00:00Z","creator_id":"1","group":{"id":"g","title":"Subitems"},
			     "column_values":[%s]}
			  ]}`,
				colJSON("person", "people", "Owner", "User 1", `{"personsAndTeams":[{"id":1,"kind":"person"},{"id":9,"kind":"team"}]}`),
				colJSON("status", "status", "Status", "Working on it", `{"index":0}`),
				colJSON("priority", "status", "Priority", "Critical ⚠️️", `{"index":3}`),
				colJSON("timeline", "timeline", "Timeline", "2026-10-01 - 2026-10-10", `{"from":"2026-10-01","to":"2026-10-10"}`),
				colJSON("tags", "tags", "Tags", "backend", `{"tag_ids":[1]}`),
				colJSON("status", "status", "Status", "Done", `{"index":1}`),
				colJSON("person", "people", "Owner", "User 2", `{"personsAndTeams":[{"id":2,"kind":"person"}]}`),
				colJSON("status", "status", "Status", "", ""),
			)
			item12 := fmt.Sprintf(`{"id":"12","name":"Chatty item","state":"active","creator_id":"1","group":{"id":"topics","title":"This week"},
			  "column_values":[],"assets":[],"updates":%s,"subitems":[]}`, manyUpdates("p1-", 100))
			fmt.Fprintf(w, `{"data":{"boards":[{"items_page":{"cursor":"cur-1","items":[%s,%s]}}]}}`, item11, item12)

		case strings.Contains(q, "next_items_page("):
			if req.Variables["cursor"] != "cur-1" {
				t.Errorf("cursor not forwarded: %v", req.Variables["cursor"])
			}
			if atomic.AddInt32(&nextHits, 1) == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, `{"errors":[{"message":"Complexity budget exhausted","extensions":{"code":"COMPLEXITY_BUDGET_EXHAUSTED","retry_in_seconds":9}}]}`)
				return
			}
			fmt.Fprintf(w, `{"data":{"next_items_page":{"cursor":null,"items":[
			  {"id":"13","name":"Last page item","state":"active","creator_id":"2","group":{"id":"done","title":"Done"},
			   "column_values":[%s],"assets":[],"updates":[],"subitems":[]}
			]}}}`, colJSON("date4", "date", "Due date", "2026-10-20", `{"date":"2026-10-20"}`))

		case strings.Contains(q, "items(ids"):
			page := int(req.Variables["page"].(float64))
			if page != 2 {
				t.Errorf("more-updates should start at page 2, got %d", page)
			}
			fmt.Fprintf(w, `{"data":{"items":[{"id":"12","updates":%s}]}}`, manyUpdates("p2-", 3))

		case strings.Contains(q, "workspaces(ids"):
			fmt.Fprint(w, `{"errors":[{"message":"User unauthorized to perform action","extensions":{"code":"UserUnauthorizedException","status_code":403}}]}`)

		default:
			t.Errorf("unexpected query: %s", q)
		}
	})
	return &nextHits, waits
}

func TestCrawl_EndToEnd(t *testing.T) {
	nextHits, waits := fakeAccount(t)
	p := New()
	snap, err := p.crawl(context.Background(), "tok", scope{BoardIDs: map[string]bool{}})
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}

	if len(snap.Users) != 101 {
		t.Fatalf("users across two pages = %d", len(snap.Users))
	}
	if len(snap.Boards) != 2 || snap.Boards[0].ID != "1" || snap.Boards[1].ID != "2" {
		t.Fatalf("boards (doc and archived must be dropped): %+v", snap.Boards)
	}
	if snap.skipped != 1 {
		t.Fatalf("skipped docs = %d", snap.skipped)
	}
	if atomic.LoadInt32(nextHits) != 2 || len(*waits) != 1 || (*waits)[0] != 9*time.Second {
		t.Fatalf("429 on page 2 should wait 9s then retry: hits=%d waits=%v", *nextHits, *waits)
	}

	byID := map[string]importProvider.SourceTask{}
	for _, st := range snap.Tasks {
		byID[st.SourceID] = st
	}
	if len(snap.Tasks) != 5 {
		t.Fatalf("tasks = %d (3 items + 2 subitems)", len(snap.Tasks))
	}
	it := byID["11"]
	if it.Status != "Working on it" || it.Priority != "Critical" || it.Completed {
		t.Fatalf("item 11 status/priority: %+v", it)
	}
	if strings.Join(it.AssigneeIds, ",") != "1" {
		t.Fatalf("assignees (team skipped) = %v", it.AssigneeIds)
	}
	if it.StartDate == nil || it.DueDate == nil || it.StartDate.Format("2006-01-02") != "2026-10-01" || it.DueDate.Format("2006-01-02") != "2026-10-10" {
		t.Fatalf("timeline dates: %v %v", it.StartDate, it.DueDate)
	}
	if strings.Join(it.Labels, ",") != "backend" {
		t.Fatalf("labels = %v", it.Labels)
	}
	if len(it.AttachmentRefs) != 1 || it.AttachmentRefs[0].SourceID != "a1" || it.AttachmentRefs[0].Size != 2048 {
		t.Fatalf("task attachments (a2 belongs to the update): %+v", it.AttachmentRefs)
	}
	if it.CommentCount != 2 || it.Metadata["monday_url"] != "https://acme.monday.com/boards/1/pulses/11" {
		t.Fatalf("comment count / url: %d %v", it.CommentCount, it.Metadata["monday_url"])
	}

	sub := byID["111"]
	if sub.ParentTaskID != "11" || sub.ProjectSourceID != "1" || sub.Status != "Done" || !sub.Completed || strings.Join(sub.AssigneeIds, ",") != "2" {
		t.Fatalf("subitem 111: %+v", sub)
	}
	if byID["112"].Status != "Subitems" {
		t.Fatalf("unset status falls back to the group title, got %q", byID["112"].Status)
	}
	if got := snap.subtasksOf["11"]; len(got) != 2 {
		t.Fatalf("subtasksOf[11] = %v", got)
	}

	if last := byID["13"]; last.DueDate == nil || last.DueDate.Format("2006-01-02") != "2026-10-20" || last.Status != "Done" {
		t.Fatalf("second-page item: %+v", last)
	}
	if n := len(snap.comments["12"]); n != 103 {
		t.Fatalf("item 12 comments across update pages = %d, want 103", n)
	}
	cs := snap.comments["11"]
	if len(cs) != 2 || cs[0].SourceID != "u1" || cs[1].SourceID != "r1" || len(cs[0].AttachmentRefs) != 1 {
		t.Fatalf("item 11 comments: %+v", cs)
	}

	if len(snap.Teams) != 2 {
		t.Fatalf("teams = %+v", snap.Teams)
	}
	prod, main := snap.Teams[0], snap.Teams[1]
	if prod.ID != "55" || prod.Name != "Product" || strings.Join(prod.MemberIDs, ",") != "1,2" {
		t.Fatalf("product team: %+v", prod)
	}
	if main.ID != mainWorkspaceID || main.Name != mainWorkspaceName || strings.Join(main.MemberIDs, ",") != "3" {
		t.Fatalf("main team: %+v", main)
	}

	plan := planFromSnapshot(snap)
	if plan.ProjectCount != 2 || plan.TaskCount != 5 || plan.SubtaskCount != 2 || plan.CommentCount != 105 || plan.FileCount != 2 || plan.FileBytes != 2058 {
		t.Fatalf("plan counts: %+v", plan)
	}
	if strings.Join(plan.StatusValues, "|") != "done|subitems|this week|working on it" {
		t.Fatalf("status values: %v", plan.StatusValues)
	}
	if strings.Join(plan.PriorityValues, "|") != "critical" {
		t.Fatalf("priority values: %v", plan.PriorityValues)
	}
	warn := strings.Join(plan.Warnings, "\n")
	for _, want := range []string{"Empty", "skipped", "subitems", "workspace members"} {
		if !strings.Contains(warn, want) {
			t.Fatalf("plan warnings missing %q:\n%s", want, warn)
		}
	}
}

func TestIterators_ServeTheCachedSnapshot(t *testing.T) {
	fakeAccount(t)
	p := New()
	snap, err := p.crawl(context.Background(), "tok", scope{BoardIDs: map[string]bool{}})
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	job := &importModels.Job{Id: uuid.New()}
	p.snapshotCache[job.Id] = snap
	ctx := context.Background()

	tasks, errCh := p.IterTasksOfProject(ctx, job, nil, "1")
	got := []string{}
	for st := range tasks {
		got = append(got, st.SourceID+":"+st.ParentTaskID)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "11:,111:11,112:11,12:,13:" {
		t.Fatalf("board 1 stream = %v", got)
	}

	subs, errCh := p.IterSubtasksOfTask(ctx, job, nil, "11")
	n := 0
	for st := range subs {
		if st.ParentTaskID != "11" {
			t.Fatalf("subtask without parent: %+v", st)
		}
		n++
	}
	if err := <-errCh; err != nil || n != 2 {
		t.Fatalf("subtasks = %d err=%v", n, err)
	}

	empty, errCh := p.IterTasksOfProject(ctx, job, nil, "2")
	for range empty {
		t.Fatal("empty board should stream nothing")
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}

	users, errCh := p.IterUsers(ctx, job, nil)
	guests := 0
	for u := range users {
		if u.IsExternal {
			guests++
		}
	}
	if err := <-errCh; err != nil || guests != 1 {
		t.Fatalf("guests = %d err=%v", guests, err)
	}

	projects, errCh := p.IterProjects(ctx, job, nil)
	var roadmap importProvider.SourceProject
	for pr := range projects {
		if pr.SourceID == "1" {
			roadmap = pr
		}
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if roadmap.TeamSourceID != "55" || strings.Join(roadmap.AdminIds, ",") != "1" || strings.Join(roadmap.MemberIds, ",") != "1,2" || roadmap.Metadata["monday_url"] != "https://acme.monday.com/boards/1" {
		t.Fatalf("roadmap project: %+v", roadmap)
	}

	p.CleanupJob(job.Id.String())
	if _, ok := p.snapshotCache[job.Id]; ok {
		t.Fatal("CleanupJob should evict the snapshot")
	}
}

func TestFetchBoards_WorkspaceScopeFilters(t *testing.T) {
	withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		ws, _ := req.Variables["ws"].([]any)
		if len(ws) != 1 || ws[0] != "55" || !strings.Contains(req.Query, "workspace_ids") {
			t.Errorf("workspace filter not sent: %v %s", req.Variables, req.Query)
		}
		fmt.Fprint(w, `{"data":{"boards":[{"id":"1","name":"A","state":"active","type":"board","workspace_id":"55"}]}}`)
	})
	boards, _, err := New().fetchBoards(context.Background(), "tok", scope{WorkspaceID: "55", BoardIDs: map[string]bool{}})
	if err != nil || len(boards) != 1 {
		t.Fatalf("boards=%v err=%v", boards, err)
	}
}

func TestFetchBoards_MainWorkspaceKeepsOnlyNullWorkspaceBoards(t *testing.T) {
	withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		fmt.Fprint(w, `{"data":{"boards":[
		  {"id":"1","name":"In main","state":"active","type":"board","workspace_id":null},
		  {"id":"2","name":"Elsewhere","state":"active","type":"board","workspace_id":"55"}]}}`)
	})
	boards, _, err := New().fetchBoards(context.Background(), "tok", scope{WorkspaceID: mainWorkspaceID, BoardIDs: map[string]bool{}})
	if err != nil || len(boards) != 1 || boards[0].ID != "1" {
		t.Fatalf("boards=%+v err=%v", boards, err)
	}
}

func TestResolveAssetURL(t *testing.T) {
	withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		ids := req.Variables["ids"].([]any)
		if ids[0] != "a1" {
			t.Errorf("asset id not sent: %v", ids)
		}
		fmt.Fprint(w, `{"data":{"assets":[{"id":"a1","public_url":"https://files.monday.test/a1?fresh=1"}]}}`)
	})
	got, err := New().resolveAssetURL(context.Background(), "tok", "a1")
	if err != nil || got != "https://files.monday.test/a1?fresh=1" {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func TestDiscover_ListsWorkspacesAndMain(t *testing.T) {
	withFakeMonday(t, func(w http.ResponseWriter, r *http.Request, req gqlReq) {
		fmt.Fprint(w, `{"data":{"workspaces":[{"id":"55","name":"Product","description":"","kind":"open"}]}}`)
	})
	items, err := New().Discover(context.Background(), uuid.New().String(), &importModels.Token{AccessToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != mainWorkspaceID || items[1].ID != "55" || items[1].Kind != "workspace" {
		t.Fatalf("discover = %+v", items)
	}
}
