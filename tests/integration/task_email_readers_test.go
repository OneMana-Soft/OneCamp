//go:build integration
// +build integration

package integration_test

// A task's emails, and its comment notices, reach only the people who may read
// the task now (PROJ-BUG-7), against real Postgres, Redis and Dgraph, through
// the real comment and status handlers.
//
// "Layoffs plan" is Maya's and Ben's. Tom commented on its "Severance letters"
// last month and has since been taken off the plan; Ravi made the task and was
// taken off too; Olu, who isn't in the plan, is the task's assignee; Sam is in
// the workspace only. A new comment emailed Tom (every earlier commenter was
// emailed), a comment mentioning Sam emailed him the comment and the task's
// name, and a status change emailed Ravi. A comment on an unassigned task Ravi
// made reached him live in his Activity.
//
// Run: go test -tags=integration ./tests/integration/ -run TestATasksEmailsReachOnlyItsReaders -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	taskController "github.com/akashc777/OneCamp/controllers/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestATasksEmailsReachOnlyItsReaders(t *testing.T) {
	ctx := context.Background()
	t.Setenv("RESEND_API_KEY", "re_integration_not_a_key") // notification email on; nothing is sent from a test
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	broker := &noticeBroker{}
	mqttInit.MqttClient = broker
	t.Cleanup(func() { mqttInit.MqttClient = nil })
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	defer search.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	defer firebaseInit.ObservePushesForTest(func(map[string]string, []string) {})()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	everyone := []string{"maya", "ben", "tom", "ravi", "olu", "sam"}
	people := map[string]uuid.UUID{}
	byID := map[string]string{}
	live := "0001-01-01T00:00:00Z"
	var nodes []map[string]any
	for _, name := range everyone {
		id := uuid.New()
		people[name], byID[id.String()] = id, name
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
		exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, 'unsub-' || $2)`, id, name)
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_deleted_at": live})
	}
	ref := func(name string) map[string]any { return map[string]any{"uid": "_:" + name} }
	tomsComment := uuid.NewString()
	earlier := func(key, by, id string) map[string]any {
		return map[string]any{"uid": "_:" + key, "dgraph.type": "Comment", "comment_uuid": id, "comment_text": "<p>Noted</p>",
			"comment_by": ref(by), "comment_task": ref("letters"), "comment_created_at": time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339), "comment_deleted_at": live}
	}
	letters, faq, plan := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Layoffs plan', $2)`, plan, people["maya"])
	exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $3, $4), ($2, $3, $4)`, letters, faq, plan, people["ravi"])
	nodes = append(nodes,
		map[string]any{"uid": "_:plan", "dgraph.type": "Project", "project_uuid": plan, "project_name": "Layoffs plan", "project_deleted_at": live,
			"project_members": []map[string]any{ref("maya"), ref("ben")}, "project_admins": []map[string]any{ref("maya")}},
		// Tom and Ben commented on it last month; Tom has since been taken off.
		map[string]any{"uid": "_:letters", "dgraph.type": "Task", "task_uuid": letters, "task_name": "Severance letters", "task_status": "todo",
			"task_deleted_at": live, "task_project": ref("plan"), "task_created_by": ref("ravi"), "task_assignee": ref("olu"),
			"task_comments": []map[string]any{earlier("byTom", "tom", tomsComment), earlier("byBen", "ben", uuid.NewString())}},
		// Nobody is assigned: its comments are told to its creator, Ravi.
		map[string]any{"uid": "_:faq", "dgraph.type": "Task", "task_uuid": faq, "task_name": "Severance FAQ", "task_status": "todo",
			"task_deleted_at": live, "task_project": ref("plan"), "task_created_by": ref("ravi")},
	)
	uids := dg.Mutate(t, nodes)

	as := func(name string) context.Context {
		t.Helper()
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, people[name])
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", name, err)
		}
		gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, people[name].String())
		if err != nil || gr == nil {
			t.Fatalf("load %s's node: %v", name, err)
		}
		return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
	}
	call := func(who string, handler http.HandlerFunc, body any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as(who)))
		return rec.Code, rec.Body.String()
	}
	comment := func(who, task, html string) (int, string) {
		t.Helper()
		code, body := call(who, taskController.CreateTaskComment, map[string]any{"task_uuid": task, "task_comment_body": html})
		var out struct {
			Data struct {
				ID string `json:"comment_id"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		if code == http.StatusOK {
			return code, out.Data.ID
		}
		return code, body
	}
	mention := func(name string) string {
		return fmt.Sprintf(`<span data-type="mention" data-id="%s@%s">@%s</span>`, people[name], uids[name], name)
	}
	// Who was emailed of the event whose dedup key holds key, of type kind.
	emailed := func(kind, key string, want int) []string {
		t.Helper()
		var got []string
		read := func() bool {
			rows, err := env.PG.Query(`SELECT user_id::text FROM notification_email_queue WHERE event_type = $1 AND strpos(dedup_key, $2) > 0`, kind, key)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			got = nil
			for rows.Next() {
				var id string
				_ = rows.Scan(&id)
				got = append(got, byID[id])
			}
			return len(got) >= want
		}
		waitFor(t, kind+" emails", read)
		time.Sleep(500 * time.Millisecond) // anyone else would be in the same batch
		read()
		slices.Sort(got)
		return got
	}
	// Who was told live in their Activity of this comment.
	told := func(commentID string) []string {
		broker.mu.Lock()
		defer broker.mu.Unlock()
		var who []string
		for _, name := range everyone {
			topic := helpers.GetMqttTopicForUserActivity(people[name].String())
			for _, m := range broker.got {
				if m.topic == topic && strings.Contains(string(m.payload), commentID) && strings.Contains(string(m.payload), `"activity_type":"COMMENT"`) {
					who = append(who, name)
					break
				}
			}
		}
		return who
	}

	t.Run("a comment is emailed to its readers, not to an earlier commenter taken off the project", func(t *testing.T) {
		code, id := comment("maya", letters, "<p>Legal signed off the amounts</p>")
		if code != http.StatusOK || id == "" {
			t.Fatalf("Maya's comment: %d %s", code, id)
		}
		if got, want := emailed("task.comment", id, 2), []string{"ben", "olu"}; !slices.Equal(got, want) {
			t.Errorf("emailed %v, want %v: Ben commented before and is in the plan, Olu is its assignee; never Tom, taken off the plan", got, want)
		}
		waitFor(t, "the comment's Activity item", func() bool { return len(told(id)) > 0 })
		if got := told(id); !slices.Equal(got, []string{"olu"}) {
			t.Errorf("told live %v, want [olu], the assignee", got)
		}
	})

	t.Run("a mention of someone outside the project is refused, and a mention of the assignee isn't", func(t *testing.T) {
		var before int
		_ = env.PG.QueryRow(`SELECT COUNT(*) FROM comments`).Scan(&before)
		code, body := comment("maya", letters, "<p>"+mention("sam")+" can you check the amounts?</p>")
		if code != http.StatusBadRequest || !strings.Contains(body, "sam isn't in this project") {
			t.Errorf("mentioning Sam: %d %s, want 400 saying he isn't in the project", code, body)
		}
		var after int
		_ = env.PG.QueryRow(`SELECT COUNT(*) FROM comments`).Scan(&after)
		if after != before {
			t.Errorf("the comment mentioning Sam was posted anyway")
		}
		code, id := comment("maya", letters, "<p>"+mention("olu")+" over to you</p>")
		if code != http.StatusOK {
			t.Fatalf("mentioning Olu, the assignee: %d %s", code, id)
		}
		if got := emailed("task.comment", id, 2); slices.Contains(got, "sam") || !slices.Contains(got, "olu") {
			t.Errorf("emailed %v: Olu, not Sam", got)
		}
		var sams int
		_ = env.PG.QueryRow(`SELECT COUNT(*) FROM notification_email_queue WHERE user_id = $1`, people["sam"]).Scan(&sams)
		if sams != 0 {
			t.Errorf("Sam, outside the project, was emailed %d times", sams)
		}
	})

	t.Run("a status change is emailed to the assignee, not to a creator taken off the project", func(t *testing.T) {
		code, body := call("maya", taskController.UpdateTaskStatus, map[string]any{"task_uuid": letters, "task_status": "inProgress", "task_project_uuid": plan})
		if code != http.StatusOK {
			t.Fatalf("Maya moving the task: %d %s", code, body)
		}
		if got, want := emailed("task.status_changed", letters, 1), []string{"olu"}; !slices.Equal(got, want) {
			t.Errorf("emailed %v, want %v: Olu is its assignee; never Ravi, who made it and was taken off the plan", got, want)
		}
	})

	t.Run("a comment on an unassigned task isn't told to a creator taken off the project", func(t *testing.T) {
		code, id := comment("ben", faq, "<p>Draft is in the folder</p>")
		if code != http.StatusOK || id == "" {
			t.Fatalf("Ben's comment: %d %s", code, id)
		}
		time.Sleep(2 * time.Second)
		if got := told(id); len(got) != 0 {
			t.Errorf("told live %v; Ravi made the task but was taken off the plan", got)
		}
	})

	t.Run("an edited comment keeps the rules a new one has", func(t *testing.T) {
		edit := func(who, commentID, html string) (int, string) {
			t.Helper()
			return call(who, taskController.UpdateTaskCommentBody, map[string]any{"task_comment_uuid": commentID, "task_uuid": letters, "task_comment_body": html})
		}
		// Tom, taken off the plan, can't edit what he wrote there.
		if code, body := edit("tom", tomsComment, "<p>Noted, and the amounts are wrong</p>"); code != http.StatusNotFound {
			t.Errorf("Tom editing his comment on a task he can't read: %d %s, want 404", code, body)
		}
		var stored struct {
			Q []struct {
				Text string `json:"comment_text"`
			} `json:"q"`
		}
		_ = json.Unmarshal(dg.Query(t, `{ q(func: eq(comment_uuid, "`+tomsComment+`")) { comment_text } }`), &stored)
		if len(stored.Q) != 1 || stored.Q[0].Text != "<p>Noted</p>" {
			t.Errorf("Tom's comment changed: %+v", stored.Q)
		}
		// An edit can't mention someone who can't see the task.
		code, id := comment("maya", letters, "<p>Checking the amounts</p>")
		if code != http.StatusOK || id == "" {
			t.Fatalf("Maya's comment: %d %s", code, id)
		}
		if code, body := edit("maya", id, "<p>"+mention("sam")+" checking the amounts</p>"); code != http.StatusBadRequest || !strings.Contains(body, "sam isn't in this project") {
			t.Errorf("Maya adding a mention of Sam: %d %s, want 400 saying he isn't in the project", code, body)
		}
		if code, body := edit("maya", id, "<p>"+mention("olu")+" checking the amounts</p>"); code != http.StatusOK {
			t.Errorf("Maya adding a mention of Olu, the assignee: %d %s", code, body)
		}
	})
}
