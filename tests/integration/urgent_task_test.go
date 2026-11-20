//go:build integration
// +build integration

package integration_test

// A task made Urgent tells its assignee at once, by push and by email
// (linear-04 item 1), against real Postgres, Redis and Dgraph, through the real
// priority and bulk handlers, in task_push_test.go's world: Maya, the
// project's admin, changes priorities. Ben, Sam and Kay hear of everything in
// "Spring launch", Mo and Tom of mentions only (a task of theirs made Urgent is
// for them), and Lee was taken off the team, his setting left behind.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestUrgentTellsTheAssigneeOnceThroughTheRoute|TestUrgentBulkTellsEachAssigneeOneDigest|TestUrgentRulesOfOneBulkChangeTellOneDigest' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	workflowBusiness "github.com/akashc777/OneCamp/business/Workflow"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	taskBulkController "github.com/akashc777/OneCamp/controllers/TaskBulk"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
)

// urgentTask is a High task in Spring launch given to assignee ("" for nobody).
func (w *taskWorld) urgentTask(t *testing.T, name, assignee string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := w.env.PG.Exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, id, w.launch, w.people["maya"]); err != nil {
		t.Fatal(err)
	}
	node := map[string]any{"uid": "_:t", "dgraph.type": "Task", "task_uuid": id, "task_name": name, "task_status": "todo",
		"task_priority": "high", "task_deleted_at": "0001-01-01T00:00:00Z", "task_project": map[string]any{"uid": w.uids["launch"]},
		"task_created_by": map[string]any{"uid": w.uids["maya"]}}
	if assignee != "" {
		node["task_assignee"] = map[string]any{"uid": w.uids[assignee]}
	}
	w.dg.Mutate(t, []map[string]any{node})
	return id
}

// post runs a handler as Maya; it must answer 200.
func (w *taskWorld) post(t *testing.T, handler http.HandlerFunc, body any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(w.asMaya))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func (w *taskWorld) prioritise(t *testing.T, task, priority string) {
	t.Helper()
	w.post(t, taskController.UpdateTaskPriority, map[string]string{"task_uuid": task, "task_project_uuid": w.launch, "task_priority": priority})
}

// urgentTold is, for one person, their shown Inbox items of tasks made Urgent,
// their Urgent emails (and of those, how many wait for a hold), and the pushes
// to their device whose title says title.
func (w *taskWorld) urgentTold(t *testing.T, name, title string) (inbox, emails, held, pushes int) {
	t.Helper()
	id := w.people[name]
	_ = w.env.PG.QueryRow(`SELECT count(*) FROM resource_notices WHERE user_uuid = $1 AND kind = 'task.urgent' AND reason <> 'listed'`, id).Scan(&inbox)
	_ = w.env.PG.QueryRow(`SELECT count(*) FROM notification_email_queue WHERE user_id = $1 AND event_type = 'task.urgent'`, id).Scan(&emails)
	_ = w.env.PG.QueryRow(`SELECT count(*) FROM notification_email_queue WHERE user_id = $1 AND event_type = 'task.urgent' AND next_retry_at > NOW()`, id).Scan(&held)
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.pushes {
		if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK && strings.Contains(p.data[firebaseInit.FIREBASE_PUSH_DATA_TITLE], title) {
			for _, tok := range p.tokens {
				if tok == "tok-"+name {
					pushes++
				}
			}
		}
	}
	return
}

// expect waits for name's counts to be want (inbox, emails, held, pushes), and
// says what they were when they weren't.
func (w *taskWorld) expect(t *testing.T, what, name, title string, want [4]int) {
	t.Helper()
	var got [4]int
	ok := within(10*time.Second, func() bool {
		got[0], got[1], got[2], got[3] = w.urgentTold(t, name, title)
		return got == want
	})
	if !ok {
		// Settled: nothing more is coming.
		got[0], got[1], got[2], got[3] = w.urgentTold(t, name, title)
		if got != want {
			t.Errorf("%s: %s told [inbox emails held pushes] %v, want %v", what, name, got, want)
		}
	}
}

func TestUrgentTellsTheAssigneeOnceThroughTheRoute(t *testing.T) {
	w := newTaskWorld(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.env.PG.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}

	kit := w.urgentTask(t, "Launch video", "ben")
	w.prioritise(t, kit, "urgent")
	w.expect(t, "made Urgent", "ben", "marked Launch video Urgent", [4]int{1, 1, 0, 1})
	w.prioritise(t, kit, "urgent")
	w.expect(t, "Urgent again", "ben", "marked Launch video Urgent", [4]int{1, 1, 0, 1})
	w.prioritise(t, kit, "high")
	w.prioritise(t, kit, "urgent")
	w.expect(t, "Urgent, High, Urgent", "ben", "marked Launch video Urgent", [4]int{2, 2, 0, 2})

	// Maya's own task, made Urgent by her: nobody.
	w.prioritise(t, w.urgentTask(t, "Maya's", "maya"), "urgent")
	// Mo hears of mentions only: a task of his made Urgent is for him.
	w.prioritise(t, w.urgentTask(t, "Mo's", "mo"), "urgent")
	// Lee was taken off the team, his setting and his task left behind: he
	// reads his own task still, but the project's news no longer reaches him.
	w.prioritise(t, w.urgentTask(t, "Lee's", "lee"), "urgent")
	// Nobody's.
	nobody := w.urgentTask(t, "Nobody's", "")
	w.prioritise(t, nobody, "urgent")
	// Kay paused her notifications: no push; the email waits for the pause.
	exec(`UPDATE users_notification_preferences SET notifications_paused_until = NOW() + interval '2 hours' WHERE user_id = $1`, w.people["kay"])
	w.prioritise(t, w.urgentTask(t, "Kay's", "kay"), "urgent")
	// Sam turned off emails of tasks assigned to him: a push alone.
	exec(`UPDATE users_notification_preferences SET email_task_assigned = false WHERE user_id = $1`, w.people["sam"])
	w.prioritise(t, w.urgentTask(t, "Sam's", "sam"), "urgent")

	w.expect(t, "her own change", "maya", "Urgent", [4]int{0, 0, 0, 0})
	w.expect(t, "mentions only", "mo", "marked Mo's Urgent", [4]int{1, 1, 0, 1})
	w.expect(t, "taken off the team", "lee", "Urgent", [4]int{0, 0, 0, 0})
	w.expect(t, "paused", "kay", "marked Kay's Urgent", [4]int{1, 1, 1, 0})
	w.expect(t, "assigned-to-you emails off", "sam", "marked Sam's Urgent", [4]int{1, 0, 0, 1})
	var any int
	_ = w.env.PG.QueryRow(`SELECT count(*) FROM resource_notices WHERE resource_uuid = $1`, nobody).Scan(&any)
	if any != 0 {
		t.Errorf("a task given to nobody told %d", any)
	}

	// The workspace's email off: a push alone.
	t.Setenv("RESEND_API_KEY", "")
	w.prioritise(t, w.urgentTask(t, "Tom's", "tom"), "urgent")
	w.expect(t, "workspace email off", "tom", "marked Tom's Urgent", [4]int{1, 0, 0, 1})
}

func TestUrgentBulkTellsEachAssigneeOneDigest(t *testing.T) {
	w := newTaskWorld(t)
	var ids []string
	for _, n := range []string{"B1", "B2", "B3", "B4", "B5"} {
		ids = append(ids, w.urgentTask(t, n, "ben"))
	}
	ids = append(ids, w.urgentTask(t, "T1", "tom"), w.urgentTask(t, "T2", "tom"), w.urgentTask(t, "Maya's", "maya"))
	w.post(t, taskBulkController.Bulk, map[string]any{"task_uuids": ids, "op": "priority", "value": "urgent"})

	// Each task its own Inbox item; one push and one email naming them.
	w.expect(t, "bulk", "ben", "marked 5 tasks Urgent", [4]int{5, 1, 0, 1})
	w.expect(t, "bulk, one task each", "ben", "marked B", [4]int{5, 1, 0, 0})
	w.expect(t, "bulk", "tom", "marked 2 tasks Urgent", [4]int{2, 1, 0, 1})
	w.expect(t, "bulk, her own", "maya", "Urgent", [4]int{0, 0, 0, 0})
	var body string
	_ = w.env.PG.QueryRow(`SELECT text_body FROM notification_email_queue WHERE user_id = $1 AND event_type = 'task.urgent'`, w.people["ben"]).Scan(&body)
	for _, n := range []string{"B1", "B2", "B3", "B4", "B5"} {
		if !strings.Contains(body, n) {
			t.Errorf("Ben's email doesn't name %s: %q", n, body)
		}
	}

	// The same change again changes nothing and tells nobody.
	w.post(t, taskBulkController.Bulk, map[string]any{"task_uuids": ids, "op": "priority", "value": "urgent"})
	w.expect(t, "bulk again", "ben", "Urgent", [4]int{5, 1, 0, 1})
}

// One bulk change whose rules make tasks Urgent (gap b): Spring launch has a
// rule that makes a task Urgent when it moves, and Maya moves three of Ben's
// tasks and one of Tom's at once. Each move runs the rule as a job of its
// own, yet all are one action: Ben is pushed and emailed once, naming his
// three, each still its own Inbox item; Tom, once, of his one.
func TestUrgentRulesOfOneBulkChangeTellOneDigest(t *testing.T) {
	w := newTaskWorld(t)
	ctx := context.Background()
	workflowBusiness.Start(ctx)
	maya := w.asMaya.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if _, err := workflowBusiness.CreateProjectRule(ctx, &maya, uuid.MustParse(w.launch), workflowBusiness.WorkflowInput{
		Name: "Moving is urgent", IsActive: true, TriggerType: workflowModel.TriggerTaskStatusChanged,
		Actions: []workflowBusiness.WorkflowAction{{Type: workflowBusiness.ActionSetPriority, Priority: "urgent"}},
	}); err != nil {
		t.Fatalf("Maya makes the rule: %v", err)
	}
	ids := []string{w.urgentTask(t, "R1", "ben"), w.urgentTask(t, "R2", "ben"), w.urgentTask(t, "R3", "ben"), w.urgentTask(t, "R4", "tom")}
	w.post(t, taskBulkController.Bulk, map[string]any{"task_uuids": ids, "op": "status", "value": "inProgress"})

	w.expect(t, "the rules of one bulk change", "ben", "3 tasks Urgent", [4]int{3, 1, 0, 1})
	w.expect(t, "the rules of one bulk change, one task each", "ben", "marked R", [4]int{3, 1, 0, 0})
	w.expect(t, "the rules of one bulk change", "tom", "R4 Urgent", [4]int{1, 1, 0, 1})
	var body string
	_ = w.env.PG.QueryRow(`SELECT text_body FROM notification_email_queue WHERE user_id = $1 AND event_type = 'task.urgent'`, w.people["ben"]).Scan(&body)
	for _, n := range []string{"R1", "R2", "R3"} {
		if !strings.Contains(body, n) {
			t.Errorf("Ben's email doesn't name %s: %q", n, body)
		}
	}
}
