package basecamp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// fakeBasecamp is one Basecamp 5 account (999) with one project (1): a to-do
// list with a group, and a card table. Answers are shaped as the bc3 API
// documents them (github.com/basecamp/bc3-api, read 19 Oct 2026), flat routes.
func fakeBasecamp(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	var srv *httptest.Server
	person := func(id int, name string) string {
		return fmt.Sprintf(`{"id":%d,"name":%q,"email_address":"%s@acme.test","personable_type":"User","client":false}`, id, name, strings.ToLower(name))
	}
	launch := func() string {
		return fmt.Sprintf(`{"id":1,"name":"Launch","description":"<div>The launch</div>","created_at":"2026-01-01T00:00:00Z","dock":[
			{"id":100,"name":"todoset","title":"To-dos","enabled":true,"url":"%[1]s/999/todosets/100.json"},
			{"id":200,"name":"kanban_board","title":"Card Table","enabled":true,"url":"%[1]s/999/card_tables/200.json"},
			{"id":300,"name":"message_board","title":"Message Board","enabled":true,"url":"%[1]s/999/message_boards/300.json"},
			{"id":400,"name":"kanban_board","title":"Old board","enabled":false,"url":"%[1]s/999/card_tables/400.json"}]}`, srv.URL)
	}
	routes := map[string]func(w http.ResponseWriter, r *http.Request){
		"/authorization.json": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"identity":{"id":1},"accounts":[
				{"product":"hey","id":5,"name":"Mail","href":"https://hey.test"},
				{"product":"bc3","id":999,"name":"Acme","href":"%s/999","app_href":"https://3.basecamp.com/999"}]}`, srv.URL)
		},
		"/999/people.json": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "" {
				// Two pages: the second is only reached through the Link header.
				w.Header().Set("Link", fmt.Sprintf(`<%s/999/people.json?page=2>; rel="next"`, srv.URL))
				fmt.Fprintf(w, `[%s,%s]`, person(10, "Ana"), person(11, "Ben"))
				return
			}
			fmt.Fprint(w, `[{"id":12,"name":"Cleo","email_address":"cleo@client.test","personable_type":"User","client":true,"company":{"id":3,"name":"Client Co"}},
				{"id":13,"name":"Gone","personable_type":"Tombstone"},
				{"id":14,"name":"Helper","personable_type":"Agent"}]`)
		},
		"/999/projects/77.json": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound) // deleted since, or hidden from the sign-in
		},
		"/999/projects.json": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("status") == "archived" {
				fmt.Fprint(w, `[{"id":2,"name":"Old site","description":"","created_at":"2025-01-01T00:00:00Z","dock":[]}]`)
				return
			}
			fmt.Fprintf(w, `[%s]`, launch())
		},
		"/999/projects/1.json": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, launch()) },
		"/999/projects/1/people.json": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `[%s,%s]`, person(10, "Ana"), person(11, "Ben"))
		},
		"/999/projects/2/people.json": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[]`) },
		"/999/todosets/100/todolists.json": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `[{"id":110,"title":"Website","completed_ratio":"1/3","todos_url":"%[1]s/999/todolists/110/todos.json","groups_url":"%[1]s/999/todolists/110/groups.json"}]`, srv.URL)
		},
		"/999/todolists/110/groups.json": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `[{"id":120,"title":"Copy","todos_url":"%s/999/todolists/120/todos.json"}]`, srv.URL)
		},
		"/999/todolists/110/todos.json": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("completed") == "true" {
				fmt.Fprintf(w, `[{"id":112,"title":"Pick a domain","completed":true,"description":"","creator":%s,"assignees":[],"comments_count":0,"subtasks_count":0,"created_at":"2026-01-02T00:00:00Z","updated_at":"2026-01-03T00:00:00Z"}]`, person(10, "Ana"))
				return
			}
			fmt.Fprintf(w, `[{"id":111,"title":"Build the page","completed":false,
				"description":"<div>See <bc-attachment sgid=\"x\" content-type=\"image/png\" url=\"https://storage.3.basecamp.com/999/blobs/1/download/shot.png\" filename=\"shot.png\"><figure><img src=\"https://storage.3.basecamp.com/999/blobs/1/download/shot.png\"></figure></bc-attachment> and ask <bc-attachment sgid=\"m\" content-type=\"application/vnd.basecamp.mention\">Ben</bc-attachment></div>",
				"description_attachments":[{"id":1,"filename":"shot.png","content_type":"image/png","byte_size":2048,"download_url":"https://storage.3.basecamp.com/999/blobs/1/download/shot.png"}],
				"starts_on":"2026-02-01","due_on":"2026-02-10","creator":%s,"assignees":[{"id":10},{"id":11}],
				"comments_count":2,"subtasks_count":1,"created_at":"2026-01-02T00:00:00Z","updated_at":"2026-01-04T00:00:00Z"}]`, person(10, "Ana"))
		},
		"/999/todolists/120/todos.json": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("completed") == "true" {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprint(w, `[{"id":121,"title":"Write the headline","completed":false,"creator":{"id":11},"assignees":[{"id":11}],"comments_count":0,"subtasks_count":0,"created_at":"2026-01-05T00:00:00Z"}]`)
		},
		"/999/card_tables/200.json": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"id":200,"title":"Card Table","lists":[
				{"id":201,"title":"Triage","type":"Kanban::Triage","cards_count":1,"cards_url":"%[1]s/999/card_tables/lists/201/cards.json"},
				{"id":202,"title":"In progress","type":"Kanban::Column","cards_count":0,"cards_url":"%[1]s/999/card_tables/lists/202/cards.json"},
				{"id":203,"title":"Done","type":"Kanban::DoneColumn","cards_count":1,"cards_url":"%[1]s/999/card_tables/lists/203/cards.json"}]}`, srv.URL)
		},
		"/999/card_tables/lists/201/cards.json": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `[{"id":211,"title":"New idea","content":"<div>From a client</div>","completed":false,"creator":{"id":12},"assignees":[],"comments_count":0,"subtasks_count":0,"created_at":"2026-01-06T00:00:00Z"}]`)
		},
		"/999/card_tables/lists/202/cards.json": func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[]`) },
		"/999/card_tables/lists/203/cards.json": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `[{"id":231,"title":"Logo","completed":false,"due_on":"2026-01-20","creator":{"id":10},"assignees":[{"id":10}],"comments_count":0,"subtasks_count":0,"created_at":"2026-01-07T00:00:00Z"}]`)
		},
		"/999/recordings/111/subtasks.json": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `[{"id":115,"title":"Hero image","completed":true,"due_on":"2026-02-05","type":"Kanban::Step","creator":{"id":10},"assignees":[{"id":11}],"created_at":"2026-01-03T00:00:00Z"}]`)
		},
		"/999/recordings/111/comments.json": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `[{"id":116,"content":"<div>Looks good</div>","creator":{"id":11},"created_at":"2026-01-04T10:00:00Z","content_attachments":[]},
				{"id":117,"content":"<div>Draft attached</div>","creator":{"id":10},"created_at":"2026-01-04T11:00:00Z",
				 "content_attachments":[{"id":2,"filename":"draft.pdf","content_type":"application/pdf","byte_size":4096,"download_url":"https://storage.3.basecamp.com/999/blobs/2/download/draft.pdf"}]}]`)
		},
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.Contains(r.Header.Get("User-Agent"), "OneCamp") {
			// Basecamp refuses a request without an app name and contact.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		h, ok := routes[r.URL.Path]
		if !ok {
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	prev := launchpadBase
	launchpadBase = srv.URL
	t.Cleanup(func() { launchpadBase = prev })
	allowAPI(t, srv.URL)
	return srv, &calls
}

func testProvider(t *testing.T) (*Provider, *importModels.Job) {
	t.Helper()
	p := New()
	p.rl = importProvider.NewSleepLimiter(1, 100)
	p.token = func(context.Context, *importModels.Job) (string, error) { return "tok", nil }
	opts, _ := json.Marshal(map[string]any{importProvider.OptBasecampAccountID: "999"})
	return p, &importModels.Job{Id: uuid.New(), Options: opts}
}

func drain[T any](t *testing.T, out <-chan T, errs <-chan error) []T {
	t.Helper()
	var got []T
	for v := range out {
		got = append(got, v)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	return got
}

// basecamp-46: everyone comes across, across pages; a client is external, an
// agent a bot, and a deleted person (a tombstone) nobody.
func TestBasecampPeople(t *testing.T) {
	fakeBasecamp(t)
	p, job := testProvider(t)
	out, errs := p.IterUsers(context.Background(), job, nil)
	users := drain(t, out, errs)
	byID := map[string]importProvider.SourceUser{}
	for _, u := range users {
		byID[u.SourceID] = u
	}
	if len(users) != 4 || byID["10"].Email != "ana@acme.test" || !byID["12"].IsExternal || !byID["14"].IsBot {
		t.Fatalf("people %+v", users)
	}
	if _, ok := byID["13"]; ok {
		t.Fatal("a tombstone isn't a person to bring across")
	}
}

// basecamp-46: projects come with their people; an archived one comes
// archived.
func TestBasecampProjects(t *testing.T) {
	fakeBasecamp(t)
	p, job := testProvider(t)
	out, errs := p.IterProjects(context.Background(), job, nil)
	projects := drain(t, out, errs)
	if len(projects) != 2 || projects[0].Name != "Launch" || projects[0].Archived || !projects[1].Archived {
		t.Fatalf("projects %+v", projects)
	}
	if strings.Join(projects[0].MemberIds, ",") != "10,11" || projects[0].Description != "<div>The launch</div>" {
		t.Fatalf("Launch %+v", projects[0])
	}
}

// basecamp-46: a project's to-dos (open and done, in lists and in groups)
// and its cards (each in its column) are its tasks. Until named lists land
// (basecamp-03) a to-do keeps its list and group as tags.
func TestBasecampTasks(t *testing.T) {
	fakeBasecamp(t)
	p, job := testProvider(t)
	out, errs := p.IterTasksOfProject(context.Background(), job, nil, "1")
	tasks := drain(t, out, errs)
	by := map[string]importProvider.SourceTask{}
	for _, st := range tasks {
		by[st.SourceID] = st
	}
	if len(tasks) != 5 {
		t.Fatalf("%d tasks: %+v", len(tasks), tasks)
	}
	build := by["111"]
	if build.Name != "Build the page" || build.Status != statusOpen || build.Completed ||
		strings.Join(build.Labels, ",") != "Website" || strings.Join(build.AssigneeIds, ",") != "10,11" ||
		build.CreatedBy != "10" || build.StartDay.IsZero() || build.DueDate != nil || build.DueDay.String() != "2026-02-10" ||
		build.CommentCount != 2 || build.SubtaskCount != 1 || build.ProjectSourceID != "1" {
		t.Fatalf("Build the page %+v", build)
	}
	if strings.Contains(build.Description, "storage.3.basecamp.com") || !strings.Contains(build.Description, "Ben") {
		t.Fatalf("a file in the text comes as a file, a mention stays as the name: %s", build.Description)
	}
	if len(build.AttachmentRefs) != 1 || build.AttachmentRefs[0].Name != "shot.png" || build.AttachmentRefs[0].Size != 2048 ||
		build.AttachmentRefs[0].Headers["Authorization"] != "" {
		t.Fatalf("files %+v", build.AttachmentRefs)
	}
	if d := by["112"]; !d.Completed || d.Status != statusDone {
		t.Fatalf("a done to-do %+v", d)
	}
	if g := by["121"]; strings.Join(g.Labels, ",") != "Website,Copy" {
		t.Fatalf("a to-do in a group keeps both: %+v", g.Labels)
	}
	if c := by["211"]; c.Status != "Triage" || c.Description != "<div>From a client</div>" || c.Completed {
		t.Fatalf("a card in triage %+v", c)
	}
	if c := by["231"]; c.Status != "Done" || !c.Completed {
		t.Fatalf("a card in the done column %+v", c)
	}
}

// basecamp-46: a to-do's steps are its sub-tasks; its comments come with
// their files.
func TestBasecampStepsAndComments(t *testing.T) {
	fakeBasecamp(t)
	p, job := testProvider(t)
	out, errs := p.IterSubtasksOfTask(context.Background(), job, nil, "111")
	steps := drain(t, out, errs)
	if len(steps) != 1 || steps[0].Name != "Hero image" || !steps[0].Completed || steps[0].ParentTaskID != "111" || strings.Join(steps[0].AssigneeIds, ",") != "11" {
		t.Fatalf("steps %+v", steps)
	}
	cout, cerrs := p.IterCommentsOfTask(context.Background(), job, nil, "111")
	comments := drain(t, cout, cerrs)
	if len(comments) != 2 || comments[0].AuthorSourceID != "11" || comments[1].Body != "<div>Draft attached</div>" ||
		len(comments[1].AttachmentRefs) != 1 || comments[1].AttachmentRefs[0].Parent.Kind != "comment" {
		t.Fatalf("comments %+v", comments)
	}
}

// basecamp-46: the plan counts what will come across and offers the card
// columns as statuses to map, before anything is written.
func TestBasecampPlan(t *testing.T) {
	fakeBasecamp(t)
	p, job := testProvider(t)
	plan, _, err := p.Plan(context.Background(), job, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ProjectCount != 2 || plan.UserCount != 4 || plan.TaskCount != 5 {
		t.Fatalf("plan %+v", plan)
	}
	statuses := strings.Join(plan.StatusValues, ",")
	for _, s := range []string{statusOpen, statusDone, "Triage", "In progress", "Done"} {
		if !strings.Contains(statuses, s) {
			t.Fatalf("statuses %s lack %s", statuses, s)
		}
	}
}

// basecamp-46: the accounts a sign-in can import are its Basecamp ones, not
// its HEY mailbox.
func TestBasecampDiscover(t *testing.T) {
	fakeBasecamp(t)
	items, err := New().Discover(context.Background(), "", &importModels.Token{AccessToken: "tok"})
	if err != nil || len(items) != 1 || items[0].ID != "999" || items[0].Name != "Acme" {
		t.Fatalf("accounts %+v (%v)", items, err)
	}
}

// basecamp-46: Basecamp asking us to slow down is said as such, and a
// refused sign-in asks to reconnect.
func TestBasecampRefusals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/people.json") {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/todos.json") {
			w.Header().Set("Retry-After", time.Now().Add(40*time.Second).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	allowAPI(t, srv.URL)
	p := New()
	p.rl = importProvider.NewSleepLimiter(1, 100)
	var people []bcPerson
	err := p.getAll(context.Background(), "tok", srv.URL+"/999/people.json", &people)
	if d, ok := importProvider.IsRateLimited(err); !ok || d.Seconds() != 7 {
		t.Fatalf("429 is %v", err)
	}
	// Retry-After may be an HTTP date as well as seconds.
	var todos []json.RawMessage
	err = p.getAll(context.Background(), "tok", srv.URL+"/999/todos.json", &todos)
	if d, ok := importProvider.IsRateLimited(err); !ok || d < 38*time.Second || d > 40*time.Second {
		t.Fatalf("429 with a date is %v (%v)", err, d)
	}
	var projects []bcProject
	if err := p.getAll(context.Background(), "tok", srv.URL+"/999/projects.json", &projects); !errors.Is(err, importProvider.ErrTokenRejected) {
		t.Fatalf("401 is %v", err)
	}
}

// basecamp-46: a file is fetched with the sign-in only from Basecamp's own
// storage; the credential is added as it is fetched, never kept with the file.
func TestBasecampFileCredential(t *testing.T) {
	own := bcFile(importProvider.SourceAttachment{URL: "https://storage.3.basecamp.com/999/blobs/1/download/a.png"}, "tok")
	other := bcFile(importProvider.SourceAttachment{URL: "https://storage.3.basecamp.com.evil.test/a.png"}, "tok")
	if own.Headers["Authorization"] != "Bearer tok" || other.Headers["Authorization"] != "" {
		t.Fatalf("own %v, other %v", own.Headers, other.Headers)
	}
}
