package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// Sprints as Jira Cloud's Sprint field (customfield_10020) gives them.
const (
	sprint11 = `{"id":11,"name":"ENG Sprint 11","state":"closed","boardId":4,"startDate":"2026-09-21T09:00:00.000Z","endDate":"2026-10-05T09:00:00.000Z","completeDate":"2026-10-05T11:30:00.000Z"}`
	sprint12 = `{"id":12,"name":"ENG Sprint 12","state":"closed","boardId":4,"startDate":"2026-10-05T09:00:00.000Z","endDate":"2026-10-19T09:00:00.000Z","completeDate":"2026-10-19T10:00:00.000Z"}`
	sprint13 = `{"id":13,"name":"ENG Sprint 13","state":"active","boardId":4,"startDate":"2026-10-19T09:00:00.000Z","endDate":"2026-10-30T09:00:00.000Z"}`
	sprint14 = `{"id":14,"name":"","state":"future","boardId":4}`
	sprint15 = `{"id":15,"name":"ENG Sprint 15","state":"future","boardId":4}`
)

// sprintIssueJSON is an issue in the given sprints, its status of the given
// category ("" for a status that comes without one).
func sprintIssueJSON(key, status, category, sprints string, subtask bool) string {
	return fmt.Sprintf(`{"key":%q,"fields":{"summary":"S","status":{"name":%q,"statusCategory":{"key":%q}},"issuetype":{"name":"Story","subtask":%t},"customfield_10020":[%s]}}`,
		key, status, category, subtask, sprints)
}

func sprintIssue(t *testing.T, key, status, category, sprints string, subtask bool) *jiraIssue {
	t.Helper()
	var iss jiraIssue
	if err := json.Unmarshal([]byte(sprintIssueJSON(key, status, category, sprints, subtask)), &iss); err != nil {
		t.Fatal(err)
	}
	return &iss
}

// An issue comes into the sprint it is in now; a finished one into the last
// sprint it was in, which finished it; an unfinished one whose sprints have
// all closed is back in the backlog, in none; a sub-task comes inside its
// task. Before, every issue came without its sprint.
func TestAnIssueComesIntoItsSprint(t *testing.T) {
	fields := siteFieldsOf(t)
	for _, c := range []struct {
		name string
		iss  *jiraIssue
		want string
	}{
		{"in the active sprint, after two it rolled over from", sprintIssue(t, "ENG-1", "In Progress", "indeterminate", sprint11+","+sprint12+","+sprint13, false), "13"},
		{"planned for a sprint that hasn't started", sprintIssue(t, "ENG-2", "To Do", "new", sprint15, false), "15"},
		{"finished in the later of two closed ones, whatever their order", sprintIssue(t, "ENG-3", "Shipped", "done", sprint12+","+sprint11, false), "12"},
		{"left unfinished when its sprints closed", sprintIssue(t, "ENG-4", "In Progress", "indeterminate", sprint11+","+sprint12, false), ""},
		{"a sub-task", sprintIssue(t, "ENG-5", "To Do", "new", sprint13, true), ""},
		{"in no sprint", sprintIssue(t, "ENG-6", "To Do", "new", "", false), ""},
	} {
		if got := buildSourceTask(c.iss, "ENG", "https://x.atlassian.net", fields).CycleSourceID; got != c.want {
			t.Errorf("%s: sprint %q, want %q", c.name, got, c.want)
		}
	}
	// A site without Jira Software has no Sprint field, and no issue a sprint.
	if got := buildSourceTask(sprintIssue(t, "ENG-7", "To Do", "new", sprint13, false), "ENG", "https://x.atlassian.net", siteFields{}).CycleSourceID; got != "" {
		t.Errorf("no Sprint field, no sprint: %q", got)
	}
}

// An issue is finished when its status's category is done, whatever the
// status is called; a status without its category goes by its name. Before,
// only a few names counted, so "Shipped" came over unfinished.
func TestAFinishedStatusIsFinishedWhateverItsName(t *testing.T) {
	for _, c := range []struct {
		status, category string
		want             bool
	}{
		{"Shipped", "done", true},
		{"In Review", "indeterminate", false},
		{"Closed", "", true},
		{"Waiting", "", false},
	} {
		iss := sprintIssue(t, "ENG-1", c.status, c.category, "", false)
		if got := buildSourceTask(iss, "ENG", "https://x.atlassian.net", siteFields{}).Completed; got != c.want {
			t.Errorf("%s (%q): finished %v, want %v", c.status, c.category, got, c.want)
		}
	}
}

// A project's sprints come as its cycles, read before its tasks: the sprints
// that hold its issues, with their names and dates, and a closed one with
// when it closed and how many issues it finished. Sprints that haven't
// started (Jira gives them no dates) follow the last, each as long as the
// latest, in the order Jira made them. The search asks only what it needs.
// Before, a project came with no cycles.
func TestAProjectsSprintsComeAsItsCycles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/rest/api/3/search/jql" || q.Get("jql") != `project = "ENG" AND cf[10020] is not EMPTY` ||
			q.Get("fields") != "customfield_10020,status,issuetype" || q.Get("expand") != "" {
			t.Errorf("request: %s", r.URL)
		}
		issues := []string{
			sprintIssueJSON("ENG-1", "Done", "done", sprint11, false),
			sprintIssueJSON("ENG-2", "Done", "done", sprint11+","+sprint12, false),
			sprintIssueJSON("ENG-3", "In Progress", "indeterminate", sprint11+","+sprint12, false),
			sprintIssueJSON("ENG-4", "In Progress", "indeterminate", sprint12+","+sprint13, false),
			sprintIssueJSON("ENG-5", "To Do", "new", sprint15, false),
			sprintIssueJSON("ENG-6", "To Do", "new", sprint14, false),
			sprintIssueJSON("ENG-7", "To Do", "new", sprint13, true),
		}
		fmt.Fprintf(w, `{"issues":[%s],"isLast":true}`, strings.Join(issues, ","))
	}))
	defer srv.Close()
	job := &importModels.Job{Id: uuid.New()}
	sp, err := New().sourceProject(context.Background(), job, "tok", srv.URL, jiraProject{Key: "ENG", Name: "Engineering"}, "", "customfield_10020")
	if err != nil {
		t.Fatal(err)
	}
	cycles := sp.Cycles
	at := func(s string) time.Time { return parseJiraTime(s) }
	closed11, closed12 := at("2026-10-05T11:30:00.000Z"), at("2026-10-19T10:00:00.000Z")
	want := []importProvider.SourceCycle{
		{SourceID: "11", Name: "ENG Sprint 11", StartsAt: at("2026-09-21T09:00:00.000Z"), EndsAt: at("2026-10-05T09:00:00.000Z"), CompletedAt: &closed11, Done: 1},
		{SourceID: "12", Name: "ENG Sprint 12", StartsAt: at("2026-10-05T09:00:00.000Z"), EndsAt: at("2026-10-19T09:00:00.000Z"), CompletedAt: &closed12, Done: 1},
		{SourceID: "13", Name: "ENG Sprint 13", StartsAt: at("2026-10-19T09:00:00.000Z"), EndsAt: at("2026-10-30T09:00:00.000Z")},
	}
	if len(cycles) != 5 {
		t.Fatalf("five sprints hold the project's issues: %+v", cycles)
	}
	for i, w := range want {
		c := cycles[i]
		if c.SourceID != w.SourceID || c.Name != w.Name || !c.StartsAt.Equal(w.StartsAt) || !c.EndsAt.Equal(w.EndsAt) || c.Done != w.Done ||
			(c.CompletedAt == nil) != (w.CompletedAt == nil) || (c.CompletedAt != nil && !c.CompletedAt.Equal(*w.CompletedAt)) {
			t.Errorf("cycle %d:\n got %+v\nwant %+v", i, c, w)
		}
	}
	// 14 then 15, in the order Jira made them, each as long as Sprint 13 and
	// from its end on (or from now, once that has passed).
	length := want[2].EndsAt.Sub(want[2].StartsAt)
	s14, s15 := cycles[3], cycles[4]
	if s14.SourceID != "14" || s14.Name != "Sprint 14" || s15.SourceID != "15" || s15.Name != "ENG Sprint 15" {
		t.Fatalf("the sprints that haven't started: %+v %+v", s14, s15)
	}
	if s14.StartsAt.Before(want[2].EndsAt) || s14.EndsAt.Sub(s14.StartsAt) != length || !s15.StartsAt.Equal(s14.EndsAt) || s15.EndsAt.Sub(s15.StartsAt) != length ||
		s14.CompletedAt != nil || s15.CompletedAt != nil {
		t.Errorf("each follows the one before, as long as the latest: %+v %+v", s14, s15)
	}
}

// A sprint with no dates starts when the latest dated one ends, or now once
// that has ended, and runs two weeks when no sprint has dates. Pure.
func TestUndatedSprintsFollowTheLast(t *testing.T) {
	// After Sprint 13 ends (30 Oct).
	now := parseJiraTime("2026-11-05T12:00:00.000Z")
	tally := func(sprints ...string) *sprintTally {
		var tl sprintTally
		for i, s := range sprints {
			tl.add(sprintIssue(t, fmt.Sprintf("ENG-%d", i+1), "To Do", "new", s, false), "customfield_10020")
		}
		return &tl
	}
	// The active sprint ends after now: the next starts at its end.
	cycles, undated := tally(sprint13, sprint15).cycles(parseJiraTime("2026-10-20T12:00:00.000Z"))
	if undated != 1 || !cycles[1].StartsAt.Equal(cycles[0].EndsAt) {
		t.Errorf("after a sprint still running: %d %+v", undated, cycles)
	}
	// It ended before now: the next starts now.
	cycles, _ = tally(sprint13, sprint15).cycles(now)
	if !cycles[1].StartsAt.Equal(now) || cycles[1].EndsAt.Sub(cycles[1].StartsAt) != cycles[0].EndsAt.Sub(cycles[0].StartsAt) {
		t.Errorf("after a sprint that has ended: %+v", cycles)
	}
	// No sprint has dates: two weeks each, from now.
	cycles, undated = tally(sprint15, sprint14).cycles(now)
	two := 14 * 24 * time.Hour
	if undated != 2 || cycles[0].SourceID != "14" || !cycles[0].StartsAt.Equal(now) || cycles[0].EndsAt.Sub(cycles[0].StartsAt) != two || !cycles[1].StartsAt.Equal(cycles[0].EndsAt) {
		t.Errorf("with no dates at all: %d %+v", undated, cycles)
	}
}

// A search Jira refuses brings the project without cycles; Jira failing on
// its side is the chunk's to try again.
func TestProjectSprintsWhenTheSearchFails(t *testing.T) {
	status := http.StatusBadRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	defer srv.Close()
	job := &importModels.Job{Id: uuid.New()}
	if cycles, err := New().projectSprints(context.Background(), job, "tok", srv.URL, "ENG", "customfield_10020"); err != nil || cycles != nil {
		t.Fatalf("a 400 comes without cycles: %v %v", cycles, err)
	}
	status = http.StatusBadGateway
	if _, err := New().projectSprints(context.Background(), job, "tok", srv.URL, "ENG", "customfield_10020"); err == nil {
		t.Fatal("a server error is the chunk's to retry")
	}
	if cycles, err := New().projectSprints(context.Background(), job, "tok", srv.URL, "ENG", ""); err != nil || cycles != nil {
		t.Fatalf("a site without the Sprint field asks nothing: %v %v", cycles, err)
	}
}

// A stage Jira stopped (a 502 on its third project) is tried again from the
// first project. The sprints of the projects read before come from the job's
// cache, neither searched nor warned about again: on a big site the searches
// before the failing project would use up the next try's rate budget, and
// the stage could stop at the same project every time. Before, each try
// searched every project again.
func TestARetriedStageReadsEachProjectsSprintsOnce(t *testing.T) {
	var mu sync.Mutex
	searches := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Query().Get("jql"), `project = "`), `"`)
		mu.Lock()
		searches[key]++
		n := searches[key]
		mu.Unlock()
		switch {
		case key == "OPS":
			w.WriteHeader(http.StatusForbidden)
		case key == "APP" && n == 1:
			w.WriteHeader(http.StatusBadGateway)
		default:
			fmt.Fprintf(w, `{"issues":[%s],"isLast":true}`, sprintIssueJSON(key+"-1", "In Progress", "indeterminate", sprint13, false))
		}
	}))
	defer srv.Close()
	p, job := New(), &importModels.Job{Id: uuid.New()}
	projects := []jiraProject{{Key: "ENG"}, {Key: "OPS"}, {Key: "APP"}}
	stage := func() ([]importProvider.SourceProject, error) {
		out := make(chan importProvider.SourceProject, len(projects))
		err := p.sendProjects(context.Background(), job, "tok", srv.URL, projects, "", "customfield_10020", out)
		close(out)
		var sent []importProvider.SourceProject
		for sp := range out {
			sent = append(sent, sp)
		}
		return sent, err
	}
	if sent, err := stage(); err == nil || len(sent) != 2 {
		t.Fatalf("Jira's 502 on APP stops the stage after ENG and OPS: %d sent, %v", len(sent), err)
	}
	sent, err := stage()
	if err != nil || len(sent) != 3 {
		t.Fatalf("the stage tried again sends all three: %d sent, %v", len(sent), err)
	}
	if len(sent[0].Cycles) != 1 || sent[0].Cycles[0].SourceID != "13" || sent[1].Cycles != nil || len(sent[2].Cycles) != 1 {
		t.Errorf("each project keeps its cycles: %+v", sent)
	}
	if searches["ENG"] != 1 || searches["OPS"] != 1 || searches["APP"] != 2 {
		t.Errorf("each project's sprints are searched once a job, APP's again after its 502: %v", searches)
	}
	// A finished job keeps nothing.
	p.CleanupJob(job.Id.String())
	if _, err := stage(); err != nil || searches["ENG"] != 2 {
		t.Errorf("after the job, its sprints are read anew: %v %v", searches, err)
	}
}
