//go:build integration

package integration_test

// WCL-1 through the handlers: each change to a task, made through the app's
// own routes, is one entry in the Work Change Log with its new value, after
// the reader's place; GET /work/changes answers each reader only what they
// may see (a project they can't view and a task kept from them never show,
// older entries included) and moves their place past the rest; a place that
// isn't one is 400; a place the log no longer has, pruned or rewound, is 410;
// and the projects programme's sweep prunes from the oldest end only.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	workSweep "github.com/akashc777/OneCamp/business/WorkSweep"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	workFeedController "github.com/akashc777/OneCamp/controllers/WorkFeed"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestWorkChangesShowEachReaderOnlyWhatTheyMaySee(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	integration.SetupRedis(t)
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	t.Cleanup(firebaseInit.ObservePushesForTest(func(map[string]string, []string) {}))

	type page struct {
		Entries []workFeed.Entry `json:"entries"`
		Cursor  string           `json:"cursor"`
	}
	read := func(who, cursor string) page {
		t.Helper()
		var p page
		if code := pastCall(t, workFeedController.Changes, http.MethodGet, "/work/changes?cursor="+cursor, nil, w.account(who), nil, &p); code != http.StatusOK {
			t.Fatalf("%s from %q: %d", who, cursor, code)
		}
		return p
	}
	status := func(who, cursor string) int {
		t.Helper()
		return pastCall(t, workFeedController.Changes, http.MethodGet, "/work/changes?"+cursor, nil, w.account(who), nil, nil)
	}
	write := func(what string, h http.HandlerFunc, body map[string]any) {
		t.Helper()
		if code := pastCall(t, h, http.MethodPost, "/", body, w.account("ana"), nil, nil); code != http.StatusOK {
			t.Fatalf("%s: %d", what, code)
		}
	}
	field := func(e workFeed.Entry, name string) any {
		t.Helper()
		var d map[string]any
		if err := json.Unmarshal(e.Diff, &d); err != nil {
			t.Fatalf("entry %d's diff: %v", e.Sequence, err)
		}
		return d[name]
	}
	ana := w.people["ana"].String()

	// Asked from nowhere, the log answers its head and nothing else.
	start := read("ana", "")
	if len(start.Entries) != 0 || start.Cursor == "" {
		t.Fatalf("from nowhere: %+v", start)
	}
	c0 := start.Cursor

	// Three writes through the app's routes: three entries, one each, in order.
	write("status", taskController.UpdateTaskStatus, map[string]any{"task_uuid": w.task, "task_status": "inProgress", "task_project_uuid": w.project})
	write("priority", taskController.UpdateTaskPriority, map[string]any{"task_uuid": w.task, "task_priority": "high", "task_project_uuid": w.project})
	write("create", taskController.CreateTask, map[string]any{"task_project_uuid": w.project, "task_name": "Release notes"})
	got := read("ana", c0)
	if len(got.Entries) != 3 {
		t.Fatalf("ana after three writes: %d entries %+v, want one per write", len(got.Entries), got.Entries)
	}
	st, pr, mk := got.Entries[0], got.Entries[1], got.Entries[2]
	for _, c := range []struct {
		e          workFeed.Entry
		kind, task string
		field      string
		want       any
	}{
		{st, workFeed.KindUpdated, w.task, "task_status", "inProgress"},
		{pr, workFeed.KindUpdated, w.task, "task_priority", "high"},
		{mk, workFeed.KindCreated, mk.ResourceUUID, "task_name", "Release notes"},
	} {
		if c.e.Kind != c.kind || c.e.ResourceUUID != c.task || c.e.ProjectUUID != w.project || c.e.Actor != ana || c.e.Source != "app" || field(c.e, c.field) != c.want {
			t.Errorf("entry %d: %+v, want %s of %s with %s %v by ana from the app", c.e.Sequence, c.e, c.kind, c.task, c.field, c.want)
		}
	}
	if _, err := uuid.Parse(mk.ResourceUUID); err != nil || mk.ResourceUUID == w.task {
		t.Errorf("the made task's entry names %q", mk.ResourceUUID)
	}
	if !(st.Sequence < pr.Sequence && pr.Sequence < mk.Sequence) || got.Cursor != strconv.FormatInt(mk.Sequence, 10) {
		t.Errorf("numbers %d %d %d, next %q: want rising, and the next place the last", st.Sequence, pr.Sequence, mk.Sequence, got.Cursor)
	}

	// Ben, in the project, sees the same; Cy (in its team, not the project)
	// and Eve (outside both) see nothing, and their place moves past it.
	if b := read("ben", c0); len(b.Entries) != 3 {
		t.Errorf("ben: %d entries, want 3", len(b.Entries))
	}
	for _, who := range []string{"cy", "eve"} {
		if p := read(who, c0); len(p.Entries) != 0 || p.Cursor != got.Cursor {
			t.Errorf("%s: %d entries, next %q; want none and %q", who, len(p.Entries), p.Cursor, got.Cursor)
		}
	}

	// The task, kept to its admins and its maker: its new change reaches Ana,
	// never Ben, and neither do its older ones.
	asAna := w.account("ana")
	kept, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, w.task, asAna.UserDgraphInfo.Uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := workAccess.Restrict(ctx, kept, asAna.UserPostgresInfo.Id, nil, time.Now()); err != nil {
		t.Fatalf("restrict: %v", err)
	}
	c1 := got.Cursor
	write("status, kept", taskController.UpdateTaskStatus, map[string]any{"task_uuid": w.task, "task_status": "inReview", "task_project_uuid": w.project})
	a1 := read("ana", c1)
	if len(a1.Entries) != 1 || a1.Entries[0].ResourceUUID != w.task || field(a1.Entries[0], "task_status") != "inReview" {
		t.Fatalf("ana after the kept task's change: %+v", a1.Entries)
	}
	if b := read("ben", c1); len(b.Entries) != 0 || b.Cursor != a1.Cursor {
		t.Errorf("ben, from whom it's kept: %+v next %q, want nothing and %q", b.Entries, b.Cursor, a1.Cursor)
	}
	if b := read("ben", c0); len(b.Entries) != 1 || b.Entries[0].Sequence != mk.Sequence {
		t.Errorf("ben from the start: %+v, want only the made task's entry", b.Entries)
	}

	// A place that isn't one is 400, and a place past the head (a log
	// rewound) is 410. A limit that isn't one asks for the most there is, as
	// every list's size does.
	head, _ := strconv.ParseInt(a1.Cursor, 10, 64)
	for _, c := range []struct {
		query string
		want  int
	}{
		{"cursor=abc", http.StatusBadRequest},
		{"cursor=" + c0 + "&limit=x", http.StatusOK},
		{"cursor=" + strconv.FormatInt(head+1000, 10), http.StatusGone},
	} {
		if code := status("ana", c.query); code != c.want {
			t.Errorf("%s: %d, want %d", c.query, code, c.want)
		}
	}

	// The sweep prunes from the oldest end: the two oldest, made over 30 days
	// ago, go; the rest stay whole; a place before them is gone (410).
	w.exec(`UPDATE task_changes SET changed_at = now() - interval '31 days' WHERE sequence IN ($1, $2)`, st.Sequence, pr.Sequence)
	if err := workSweep.WorkChangesPrune(ctx); err != nil {
		t.Fatal(err)
	}
	if code := status("ana", "cursor="+c0); code != http.StatusGone {
		t.Errorf("from before what was pruned: %d, want 410", code)
	}
	if p := read("ana", strconv.FormatInt(pr.Sequence, 10)); len(p.Entries) != 2 || p.Entries[0].Sequence != mk.Sequence {
		t.Errorf("from just before what's kept: %+v, want the made task's and the kept task's", p.Entries)
	}
	// All of it old: all but the newest go. The place after the made task's
	// entry still reads the newest (nothing it hadn't read went); the place
	// before it, which read both a moment ago, is gone now (410).
	w.exec(`UPDATE task_changes SET changed_at = now() - interval '31 days'`)
	if err := workSweep.WorkChangesPrune(ctx); err != nil {
		t.Fatal(err)
	}
	var left int
	var newest int64
	if err := w.env.PG.QueryRow(`SELECT count(*), max(sequence) FROM task_changes`).Scan(&left, &newest); err != nil || left != 1 || newest != head {
		t.Fatalf("after the whole log aged: %d left, newest %d (err %v); want only %d", left, newest, err, head)
	}
	if p := read("ana", strconv.FormatInt(mk.Sequence, 10)); len(p.Entries) != 1 || p.Entries[0].Sequence != head || p.Cursor != a1.Cursor {
		t.Errorf("from the made task's entry, all but the newest pruned: %+v, want only the newest", p)
	}
	if code := status("ana", "cursor="+strconv.FormatInt(pr.Sequence, 10)); code != http.StatusGone {
		t.Errorf("from before the made task's entry, now pruned: %d, want 410", code)
	}
	if p := read("ana", a1.Cursor); len(p.Entries) != 0 || p.Cursor != a1.Cursor {
		t.Errorf("from the head: %+v", p)
	}
}
