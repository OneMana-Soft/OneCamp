//go:build integration
// +build integration

package integration_test

// A task's pushes, and the desktop app's notices beside them, against real
// Postgres, Redis and Dgraph, through the real task and comment handlers.
//
// "Spring launch" is Maya's project, with Ben, Olu, Sam, Mo, Tom, Kay and Dex
// in it, and Lee, who was in it until he was taken off its team: that takes
// someone off the team's projects but leaves their setting for each project
// behind. Ben, Tom and Lee commented on "Press kit" before; Olu is its assignee
// and uses only the desktop app (no device). Everyone's setting for the project
// is the default, everything, but Mo's and Tom's, which are mentions only. Dex
// has been deactivated. "Quiet room" is Maya's and Mo's.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestATaskCommentReachesWhomItIsFor|TestANewTaskReachesItsReadersAndWhomItIsGivenTo' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	taskController "github.com/akashc777/OneCamp/controllers/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// taskWorld is the projects above, what was pushed in them and who heard.
type taskWorld struct {
	env                *integration.Env
	broker             *noticeBroker
	people             map[string]uuid.UUID
	uids               map[string]string
	launch, quiet, kit string
	asMaya             context.Context

	mu     sync.Mutex
	pushes []taskPush
}

type taskPush struct {
	data   map[string]string
	tokens []string
}

func newTaskWorld(t *testing.T) *taskWorld {
	t.Helper()
	ctx := context.Background()
	t.Setenv("RESEND_API_KEY", "re_integration_not_a_key") // notification email on; nothing is sent from a test
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	w := &taskWorld{env: integration.SetupEnv(t), broker: &noticeBroker{}, people: map[string]uuid.UUID{}}
	if err := postgresInit.ConnectPostgres(ctx, w.env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.UseMqttClient(t, w.broker)
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		fmt.Fprint(rw, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	t.Cleanup(firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.pushes = append(w.pushes, taskPush{data, tokens})
	}))

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	everyone := []string{"maya", "ben", "olu", "sam", "mo", "tom", "kay", "dex", "lee"}
	live := "0001-01-01T00:00:00Z"
	var nodes []map[string]any
	for _, name := range everyone {
		id := uuid.New()
		w.people[name] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
		exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, 'unsub-' || $2)`, id, name)
		if name != "olu" { // Olu uses only the desktop app: no device
			exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-' || $2)`, id, name)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_deleted_at": live})
	}
	ref := func(name string) map[string]any { return map[string]any{"uid": "_:" + name} }
	refs := func(names ...string) []map[string]any {
		var out []map[string]any
		for _, n := range names {
			out = append(out, ref(n))
		}
		return out
	}
	earlier := func(by string) map[string]any {
		return map[string]any{"uid": "_:by" + by, "dgraph.type": "Comment", "comment_uuid": uuid.NewString(), "comment_text": "<p>Noted</p>",
			"comment_by": ref(by), "comment_task": ref("kit"), "comment_created_at": time.Now().Add(-7 * 24 * time.Hour).Format(time.RFC3339), "comment_deleted_at": live}
	}
	w.kit, w.launch, w.quiet = uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Spring launch', $3), ($2, 'Quiet room', $3)`, w.launch, w.quiet, w.people["maya"])
	exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, w.kit, w.launch, w.people["maya"])
	for _, name := range everyone {
		setting := "all"
		if name == "mo" || name == "tom" {
			setting = "mention"
		}
		exec(`INSERT INTO users_project_notification (user_id, project_id, notification_type) VALUES ($1, $2, $3)`, w.people[name], w.launch, setting)
	}
	exec(`INSERT INTO users_project_notification (user_id, project_id, notification_type) VALUES ($1, $3, 'all'), ($2, $3, 'mention')`, w.people["maya"], w.people["mo"], w.quiet)
	inLaunch := refs("maya", "ben", "olu", "sam", "mo", "tom", "kay", "dex") // never Lee, taken off the team
	nodes = append(nodes,
		map[string]any{"uid": "_:team", "dgraph.type": "Team", "team_uuid": uuid.NewString(), "team_name": "Launch",
			"team_members": inLaunch, "team_admins": refs("maya")},
		map[string]any{"uid": "_:launch", "dgraph.type": "Project", "project_uuid": w.launch, "project_name": "Spring launch", "project_deleted_at": live,
			"project_team": ref("team"), "project_members": inLaunch, "project_admins": refs("maya"), "project_created_by": ref("maya")},
		map[string]any{"uid": "_:quiet", "dgraph.type": "Project", "project_uuid": w.quiet, "project_name": "Quiet room", "project_deleted_at": live,
			"project_team": ref("team"), "project_members": refs("maya", "mo"), "project_admins": refs("maya"), "project_created_by": ref("maya")},
		map[string]any{"uid": "_:kit", "dgraph.type": "Task", "task_uuid": w.kit, "task_name": "Press kit", "task_status": "todo",
			"task_deleted_at": live, "task_project": ref("launch"), "task_created_by": ref("maya"), "task_assignee": ref("olu"),
			"task_comments": []map[string]any{earlier("ben"), earlier("tom"), earlier("lee")}},
	)
	w.uids = dg.Mutate(t, nodes)
	exec(`UPDATE users SET deleted_at = NOW() WHERE id = $1`, w.people["dex"])

	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, w.people["maya"])
	if err != nil || pg == nil {
		t.Fatalf("load maya: %v", err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, w.people["maya"].String())
	if err != nil || gr == nil {
		t.Fatalf("load maya's node: %v", err)
	}
	w.asMaya = context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	return w
}

func (w *taskWorld) mention(name string) string {
	return fmt.Sprintf(`<span data-type="mention" data-id="%s@%s">@%s</span>`, w.people[name], w.uids[name], name)
}

// call runs a task handler as Maya and answers its data.
func (w *taskWorld) call(t *testing.T, handler http.HandlerFunc, body any) map[string]string {
	t.Helper()
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(w.asMaya))
	var out struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	data := map[string]string{}
	for k, v := range out.Data {
		if s, ok := v.(string); ok {
			data[k] = s
		}
	}
	return data
}

// pushedTo is the devices pushed of the kind of thing (type) with this id
// (its type_id, or for a comment its thread), sorted.
func (w *taskWorld) pushedTo(kind, id string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var devices []string
	for _, p := range w.pushes {
		if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == kind &&
			(p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] == id || p.data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] == id) {
			devices = append(devices, p.tokens...)
		}
	}
	slices.Sort(devices)
	return devices
}

// noticed is who the desktop notice with this id reached.
func (w *taskWorld) noticed(noticeID string) []string {
	byName := map[string]userModels.UserInfo{}
	for name, id := range w.people {
		byName[name] = userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: id.String()}}
	}
	return w.broker.notices(noticeID, byName)
}

// within is whether cond came true in time; whatever else was coming has
// come a second later.
func within(d time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if cond() {
			time.Sleep(time.Second)
			return true
		}
	}
	return false
}

// The comment's push looked devices up for itself, by the project's settings
// alone:
//   - nobody's desktop app heard of a task comment (TN-BUG-12), Olu's included;
//   - someone who asked to hear of everything was pushed only the threads they
//     had commented in, so less than someone who asked for mentions only: Sam,
//     mentioned, and Olu, given the task, heard nothing;
//   - Lee, off the project, was pushed the comment by the setting left behind.
func TestATaskCommentReachesWhomItIsFor(t *testing.T) {
	w := newTaskWorld(t)
	out := w.call(t, taskController.CreateTaskComment, map[string]any{"task_uuid": w.kit,
		"task_comment_body": "<p>Draft is in the drive " + w.mention("sam") + " " + w.mention("mo") + " " + w.mention("dex") + "</p>"})
	if !within(20*time.Second, func() bool { return len(w.pushedTo("task_comment", w.kit)) > 0 }) {
		t.Fatal("the comment was pushed to nobody")
	}
	if got, want := w.pushedTo("task_comment", w.kit), []string{"tok-ben", "tok-mo", "tok-sam"}; !slices.Equal(got, want) {
		t.Errorf("the comment was pushed to %v, want %v: Ben commented before, Sam and Mo are mentioned; never Maya, who wrote it, "+
			"Tom (mentions only, not mentioned), Kay (not in the thread), Dex (deactivated) or Lee (off the project); Olu has no device", got, want)
	}
	if got, want := w.noticed("task_comment:"+w.kit+":"+out["comment_id"]), []string{"ben", "mo", "olu", "sam"}; !slices.Equal(got, want) {
		t.Errorf("the comment's desktop notice reached %v, want %v: Olu is given the task and uses only the desktop app", got, want)
	}
}

// A new task is told to its project's people by their setting, and to whom
// it's given. It asked only the project's settings who was in it, so Lee, off
// the project, was pushed the new task's name and description by the setting
// left behind; and when nobody's setting told them of it, it told nobody at
// all, not even whom it was given to, who wasn't emailed either.
func TestANewTaskReachesItsReadersAndWhomItIsGivenTo(t *testing.T) {
	w := newTaskWorld(t)

	t.Run("its project's people who may read it, never someone off it", func(t *testing.T) {
		task := w.call(t, taskController.CreateTask, map[string]any{"task_project_uuid": w.launch, "task_name": "Venue contract",
			"task_description": "<p>Needs a second pair of eyes " + w.mention("sam") + "</p>", "task_assignee_uuid": w.people["olu"].String()})["task_uuid"]
		if !within(20*time.Second, func() bool { return len(w.pushedTo("task", task)) > 0 }) {
			t.Fatal("the new task was pushed to nobody")
		}
		if got, want := w.pushedTo("task", task), []string{"tok-ben", "tok-kay", "tok-sam"}; !slices.Equal(got, want) {
			t.Errorf("the new task was pushed to %v, want %v: everyone hearing of everything, but Maya, who made it, "+
				"Dex (deactivated) and Lee (off the project); Mo and Tom hear of mentions only; Olu has no device", got, want)
		}
		if got, want := w.noticed("task:"+task), []string{"ben", "kay", "olu", "sam"}; !slices.Equal(got, want) {
			t.Errorf("the new task's desktop notice reached %v, want %v: Olu is given it and uses only the desktop app", got, want)
		}
	})

	t.Run("whom it's given to, when nobody's setting tells them of it", func(t *testing.T) {
		task := w.call(t, taskController.CreateTask, map[string]any{"task_project_uuid": w.quiet, "task_name": "Soundcheck",
			"task_assignee_uuid": w.people["mo"].String()})["task_uuid"]
		if !within(20*time.Second, func() bool { return len(w.pushedTo("task", task)) > 0 }) {
			t.Error("the new task wasn't pushed to Mo, who it's given to: Mo hears of mentions only, and so does nobody else")
		} else if got := w.pushedTo("task", task); !slices.Equal(got, []string{"tok-mo"}) {
			t.Errorf("the new task was pushed to %v, want Mo's device", got)
		}
		if got := w.noticed("task:" + task); !slices.Equal(got, []string{"mo"}) {
			t.Errorf("the new task's desktop notice reached %v, want Mo", got)
		}
		emailed := func() bool {
			var n int
			_ = w.env.PG.QueryRow(`SELECT count(*) FROM notification_email_queue WHERE user_id = $1 AND event_type = 'task.assigned' AND strpos(dedup_key, $2) > 0`,
				w.people["mo"], task).Scan(&n)
			return n > 0
		}
		if !within(10*time.Second, emailed) {
			t.Error("Mo wasn't emailed the task given to them")
		}
	})
}
