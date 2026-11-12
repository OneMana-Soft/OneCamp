//go:build integration
// +build integration

package integration_test

// A move of data tells nobody (NOTIF-H6): tasks GitHub's import and its
// webhook make in a project are its tasks already, so nobody hearing of
// everything in it is pushed, sent the desktop app's notice, emailed or told
// in their Inbox of each. Whom an issue is assigned to on GitHub, when that
// login is a OneCamp person's, is told once; a comment written on GitHub, and
// a task a person makes, are told as ever.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	taskAdapter "github.com/akashc777/OneCamp/adapter/Task"
	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestADataMoveTellsNobody(t *testing.T) {
	refuseGitHub(t) // beneath the one stand-in below: nothing reaches GitHub itself
	w := newTaskWorld(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// acme/quiet-move is linked to Spring launch by Maya: its issues sync, and
	// a new one makes a task. Ben's GitHub login is his. The repository is this
	// test's own: links are cached by repository for five minutes, so a link
	// left on another test's repository (github_code_refs_test.go signs acme/web
	// with a secret of its own) would answer that test's deliveries.
	var link uuid.UUID
	if err := w.env.PG.QueryRow(`INSERT INTO github_links (project_id, repo_owner, repo_name, sync_issues, sync_prs, auto_create_tasks, created_by)
		VALUES ($1, 'acme', 'quiet-move', true, true, true, $2) RETURNING id`, w.launch, w.people["maya"]).Scan(&link); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO integrations (entity_type, entity_id, provider, access_token) VALUES ('org', $1, 'github', 'test-token')`, uuid.Nil)
	benLogin, kayLogin := "ben-"+uuid.NewString()[:8], "kay-"+uuid.NewString()[:8]
	for name, login := range map[string]string{"ben": benLogin, "kay": kayLogin} {
		exec(`INSERT INTO integrations (entity_type, entity_id, provider, metadata) VALUES ('user', $1, 'github', $2)`, w.people[name],
			fmt.Sprintf(`{"github_login": %q, "user_uuid": %q}`, login, w.people[name].String()))
	}
	repo := map[string]any{"full_name": "acme/quiet-move", "name": "quiet-move", "owner": map[string]any{"login": "acme"}}
	issueURL := func(n int) string { return fmt.Sprintf("https://github.com/acme/quiet-move/issues/%d", n) }
	hook := func(event string, payload map[string]any) {
		t.Helper()
		payload["repository"] = repo
		raw, _ := json.Marshal(payload)
		if err := githubBusiness.HandleGitHubWebhookEvent(ctx, event, raw); err != nil {
			t.Fatalf("GitHub's %s event: %v", event, err)
		}
	}
	taskOf := func(n int) string {
		var id string
		_ = w.env.PG.QueryRow(`SELECT id FROM tasks WHERE github_issue_url = $1`, issueURL(n)).Scan(&id)
		return id
	}
	// told is how each of the ways of telling reached the people of a task.
	told := func(task string) (pushed, noticed []string, emails, inbox int) {
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM notification_email_queue WHERE strpos(dedup_key, $1) > 0`, task).Scan(&emails); err != nil {
			t.Fatal(err)
		}
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM resource_notices WHERE resource_uuid = $1`, task).Scan(&inbox); err != nil {
			t.Fatal(err)
		}
		return w.pushedTo("task", task), w.noticed("task:" + task), emails, inbox
	}

	// The import of three open issues, and GitHub's webhook opening three more.
	var issues []map[string]any
	for n := 1; n <= 3; n++ {
		issues = append(issues, map[string]any{"number": n, "title": fmt.Sprintf("Imported %d", n), "body": "", "state": "open", "html_url": issueURL(n)})
	}
	listed, _ := json.Marshal(issues)
	// Issue #3 as Sam's refresh finds it below: closed, given to Kay, with two
	// comments.
	issue3, _ := json.Marshal(map[string]any{"number": 3, "title": "Imported 3", "body": "", "state": "closed", "state_reason": "completed",
		"html_url": issueURL(3), "assignee": map[string]any{"login": kayLogin}})
	comments3, _ := json.Marshal([]map[string]any{
		{"id": 9031, "body": "Fixed in #40", "user": map[string]any{"login": benLogin}, "created_at": "2026-11-05T10:00:00Z"},
		{"id": 9032, "body": "Verified on staging", "user": map[string]any{"login": kayLogin}, "created_at": "2026-11-05T11:00:00Z"}})
	// GitHub is this one stand-in from before the first hook to the end, so no
	// request reaches api.github.com (a second would wrap it, and its 404s
	// shadow these bodies). octo has no profile there (a 404).
	fakeGitHub(t, map[string]string{"/repos/acme/quiet-move/issues": string(listed),
		"/repos/acme/quiet-move/issues/3": string(issue3), "/repos/acme/quiet-move/issues/3/comments": string(comments3)})
	githubBusiness.StartGitHubImportWorker(ctx)
	if _, err := githubBusiness.EnqueueImportIssues(ctx, link, w.people["maya"]); err != nil {
		t.Fatal(err)
	}
	for n := 11; n <= 13; n++ {
		hook("issues", map[string]any{"action": "opened", "sender": map[string]any{"login": "octo"},
			"issue": map[string]any{"number": n, "title": fmt.Sprintf("Opened %d", n), "body": "", "state": "open", "html_url": issueURL(n)}})
	}
	made := func() bool {
		for _, n := range []int{1, 2, 3, 11, 12, 13} {
			if taskOf(n) == "" {
				return false
			}
		}
		return true
	}
	if !within(60*time.Second, made) {
		t.Fatal("the import and the webhook didn't make their six tasks")
	}
	time.Sleep(3 * time.Second) // whatever telling was coming has come

	for _, n := range []int{1, 2, 3, 11, 12, 13} {
		how := "GitHub's import"
		if n > 10 {
			how = "GitHub's webhook"
		}
		task := taskOf(n)
		// GitHub has no priority to carry over: the task has none (CR-B61 M20).
		if got, err := taskBusiness.GetDgraphTaskInfo(helpers.WithSystemRead(ctx), task, w.uids["maya"]); err != nil || got == nil {
			t.Errorf("the task %s made of issue #%d: %v", how, n, err)
		} else if got.Priority != "" {
			t.Errorf("the task %s made of issue #%d has priority %q, want none", how, n, got.Priority)
		}
		pushed, noticed, emails, inbox := told(task)
		if len(pushed) > 0 || len(noticed) > 0 || emails > 0 || inbox > 0 {
			t.Errorf("the task %s made of issue #%d was told: pushed to %v, desktop notice to %v, %d emails, %d Inbox notices; want nobody told",
				how, n, pushed, noticed, emails, inbox)
		}
	}

	// Whoever assigned an issue on GitHub (the event's sender, as the person
	// they are here) is who acted, never the link's maker (Maya), who did
	// nothing: their own assignment tells nobody, anyone else's tells whom
	// it's given to once, as theirs, and the task.assigned event names them.
	// A sender with no account here is provisioned as an external person, as
	// a GitHub commenter is, and named by their login.
	name := func(who string) string {
		u, err := userDomain.GetDgraphUserInfoByUUID(ctx, w.people[who].String())
		if err != nil || u == nil || u.DisplayName() == "" {
			t.Fatalf("%s's name: %v", who, err)
		}
		return u.DisplayName()
	}
	var heardMu sync.Mutex
	assignedBy := map[string][]any{} // task -> the assigned_by of each task.assigned event
	webhookBusiness.RegisterEventListener(func(_ context.Context, eventType string, data map[string]interface{}) {
		if eventType == "task.assigned" {
			heardMu.Lock()
			assignedBy[fmt.Sprint(data["task_id"])] = append(assignedBy[fmt.Sprint(data["task_id"])], data["assigned_by"])
			heardMu.Unlock()
		}
	})
	eventsOf := func(task string) []any {
		heardMu.Lock()
		defer heardMu.Unlock()
		return slices.Clone(assignedBy[task])
	}
	// subjects is each task.assigned email of the task to who.
	subjects := func(who, task string) []string {
		rows, err := w.env.PG.Query(`SELECT subject FROM notification_email_queue WHERE user_id = $1 AND event_type = 'task.assigned' AND strpos(dedup_key, $2) > 0`,
			w.people[who], task)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	assign := func(n int, sender, assignee string) map[string]any {
		payload := map[string]any{"action": "assigned", "sender": map[string]any{"login": sender},
			"issue": map[string]any{"number": n, "title": fmt.Sprintf("Issue %d", n), "body": "", "state": "open", "html_url": issueURL(n),
				"assignee": map[string]any{"login": assignee}}}
		hook("issues", payload)
		return payload
	}

	t.Run("whom an issue is assigned to on GitHub is told once, as the act of whoever assigned it", func(t *testing.T) {
		// octo, with no account here, gives #11 to Ben: octo is provisioned as
		// an external person (GitHub has no profile of them), and Ben is told
		// once that octo did it.
		task := taskOf(11)
		again := assign(11, "octo", benLogin)
		if !within(10*time.Second, func() bool { return len(subjects("ben", task)) > 0 }) {
			t.Fatal("Ben, whom the issue is assigned to on GitHub, wasn't told")
		}
		hook("issues", again) // GitHub sends it again
		time.Sleep(2 * time.Second)
		if got, want := subjects("ben", task), []string{"octo assigned a task to you"}; !slices.Equal(got, want) {
			t.Errorf("Ben, given #11 on GitHub by octo, was told %q; want %q once, never naming Maya, who did nothing", got, want)
		}
		var octo string
		if err := w.env.PG.QueryRow(`SELECT id FROM users WHERE LOWER(github_login) = 'octo' AND deleted_at IS NULL`).Scan(&octo); err != nil {
			t.Fatalf("octo, who gave #11 to Ben on GitHub, isn't a person here: %v", err)
		}
		if got := eventsOf(task); len(got) == 0 || slices.ContainsFunc(got, func(by any) bool { return by != octo }) {
			t.Errorf("the task.assigned events of #11 say it was assigned by %v; want octo %s each time, never Maya %s", got, octo, w.people["maya"])
		}

		// Ben gives #13 to himself on GitHub: his own act tells him nothing.
		task = taskOf(13)
		assign(13, benLogin, benLogin)
		if !within(10*time.Second, func() bool { return len(eventsOf(task)) > 0 }) {
			t.Fatal("#13, which Ben gave himself on GitHub, made no task.assigned event: was it assigned at all?")
		}
		if got := subjects("ben", task); len(got) > 0 {
			t.Errorf("Ben, who gave #13 to himself on GitHub, was told %q; want nothing of his own act", got)
		}

		// Kay gives #1 to Ben: Ben is told it was Kay, and so is the event.
		task = taskOf(1)
		assign(1, kayLogin, benLogin)
		if !within(10*time.Second, func() bool { return len(subjects("ben", task)) > 0 }) {
			t.Fatal("Ben, whom Kay gave #1 to on GitHub, wasn't told")
		}
		if got, want := subjects("ben", task), []string{name("kay") + " assigned a task to you"}; !slices.Equal(got, want) {
			t.Errorf("Ben, whom Kay gave #1 to on GitHub, was told %q; want %q", got, want)
		}
		if got, want := eventsOf(task), []any{w.people["kay"].String()}; !slices.Equal(got, want) {
			t.Errorf("the task.assigned event of #1 says it was assigned by %v; want Kay %v", got, want)
		}

		// Kay gives #2 to Maya, the link's maker: Maya is told once.
		task = taskOf(2)
		mayaLogin := "maya-" + uuid.NewString()[:8]
		if _, err := w.env.PG.Exec(`INSERT INTO integrations (entity_type, entity_id, provider, metadata) VALUES ('user', $1, 'github', $2)`, w.people["maya"],
			fmt.Sprintf(`{"github_login": %q, "user_uuid": %q}`, mayaLogin, w.people["maya"].String())); err != nil {
			t.Fatal(err)
		}
		assign(2, kayLogin, mayaLogin)
		if !within(10*time.Second, func() bool { return len(subjects("maya", task)) > 0 }) {
			t.Fatal("Maya, the link's maker, whom Kay gave #2 to on GitHub, wasn't told")
		}
		if got, want := subjects("maya", task), []string{name("kay") + " assigned a task to you"}; !slices.Equal(got, want) {
			t.Errorf("Maya, whom Kay gave #2 to on GitHub, was told %q; want %q once", got, want)
		}
	})

	// A comment is pushed to whoever commented on the task before, whom it
	// names and whom the task is given to: Kay wrote on issue #12 on GitHub
	// first, so Ben's comment there reaches her. (Issue #12 has nobody of its
	// own, so the first comment, from GitHub or made here, reaches nobody.)
	t.Run("a comment written on GitHub is told as ever", func(t *testing.T) {
		task := taskOf(12)
		comment := func(id int, login, body string) {
			hook("issue_comment", map[string]any{"action": "created",
				"issue":   map[string]any{"number": 12, "html_url": issueURL(12)},
				"comment": map[string]any{"id": id, "body": body, "user": map[string]any{"login": login}}})
		}
		comment(9012, kayLogin, "Can't reproduce on Chrome")
		comment(9013, benLogin, "Seen on Safari too")
		if !within(20*time.Second, func() bool { return len(w.pushedTo("task_comment", task)) > 0 }) {
			t.Fatal("the comment written on GitHub was pushed to nobody; want it told as a comment made here is")
		}
		if got, want := w.pushedTo("task_comment", task), []string{"tok-kay"}; !slices.Equal(got, want) {
			t.Errorf("Ben's comment written on GitHub was pushed to %v, want %v: Kay commented before; never Ben, who wrote it", got, want)
		}
	})

	// What GitHub's refresh brings was there already: Sam's refresh of #3,
	// given to Ben, takes the issue's state (closed), its assignee (Kay) and
	// its two comments (Ben's, then Kay's), and tells nobody: no email (the
	// status Ben was told of, the assignment Kay was), no push (Kay's comment
	// reached Ben, who commented before and is given the task), nothing live
	// (Kay's comment was Ben's activity item) and no Inbox notice.
	t.Run("what GitHub's refresh brings tells nobody", func(t *testing.T) {
		task := taskOf(3)
		assign(3, "octo", benLogin)
		if !within(10*time.Second, func() bool { return len(subjects("ben", task)) > 0 }) {
			t.Fatal("#3 wasn't given to Ben before the refresh")
		}
		sam, err := userDomain.GetDgraphUserInfoByUUID(ctx, w.people["sam"].String())
		if err != nil || sam == nil {
			t.Fatalf("Sam's node: %v", err)
		}

		pushedBefore, noticedBefore, emailsBefore, inboxBefore := told(task)
		commentPushesBefore, since := w.pushedTo("task_comment", task), w.broker.mark()
		if err := githubBusiness.RefreshFromGitHub(ctx, uuid.MustParse(task), true, sam); err != nil {
			t.Fatalf("Sam's refresh of #3: %v", err)
		}
		var brought int
		synced := func() bool {
			_ = w.env.PG.QueryRow(`SELECT count(*) FROM github_comment_mappings WHERE task_uuid = $1`, task).Scan(&brought)
			return brought == 2
		}
		if !within(20*time.Second, synced) {
			t.Fatalf("the refresh brought %d of #3's 2 comments", brought)
		}
		now, err := taskBusiness.GetDgraphTaskInfo(helpers.WithSystemRead(ctx), task, "")
		if err != nil || now == nil || now.Assignee == nil || now.Assignee.Uuid != w.people["kay"].String() || now.Status != "done" {
			t.Fatalf("after the refresh #3 isn't done and given to Kay %s, as the issue is (%v)", w.people["kay"], err)
		}
		time.Sleep(3 * time.Second) // whatever telling was coming has come

		pushed, noticed, emails, inbox := told(task)
		if emails != emailsBefore {
			t.Errorf("Sam's refresh of #3 emailed %d times (the status GitHub closed it to, the assignment to Kay); want nobody emailed", emails-emailsBefore)
		}
		if len(pushed) != len(pushedBefore) || len(noticed) != len(noticedBefore) || inbox != inboxBefore {
			t.Errorf("Sam's refresh of #3 told of the task: pushed to %v (before %v), desktop notice to %v (before %v), %d Inbox notices more; want nobody told",
				pushed, pushedBefore, noticed, noticedBefore, inbox-inboxBefore)
		}
		if got := w.pushedTo("task_comment", task); len(got) != len(commentPushesBefore) {
			t.Errorf("the comments Sam's refresh brought from GitHub were pushed to %v (before %v); want nobody pushed: they were written there already",
				got, commentPushesBefore)
		}
		if got := w.broker.heardOf(task, since, w.people); len(got) > 0 {
			t.Errorf("Sam's refresh of #3 reached %v live (an activity item, a desktop notice); want nobody", got)
		}
	})

	// A batch a move of data makes tells nobody, as one task it makes doesn't
	// (CreateTasks, QuietMove): Ben, Kay and Sam hear of every new task here.
	t.Run("a batch a move of data makes tells nobody", func(t *testing.T) {
		maya := w.asMaya.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
		project, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, w.launch, maya.UserDgraphInfo.Uid)
		if err != nil || project == nil {
			t.Fatalf("the project: %v", err)
		}
		ids, err := taskBusiness.CreateTasks(taskBusiness.QuietMove(ctx), uuid.MustParse(w.launch), &maya, project,
			[]taskBusiness.NewTask{{Input: taskAdapter.CreateOrUpdateTaskInput{TaskName: "Moved in: the venue shortlist", ProjectUuid: w.launch}, Parent: -1}})
		if err != nil || len(ids) != 1 {
			t.Fatalf("CreateTasks: %v %v", ids, err)
		}
		time.Sleep(3 * time.Second) // whatever telling was coming has come
		task := ids[0].String()
		if pushed, noticed, emails, inbox := told(task); len(pushed) > 0 || len(noticed) > 0 || emails > 0 || inbox > 0 {
			t.Errorf("the task a move of data made in a batch was told: pushed to %v, desktop notice to %v, %d emails, %d Inbox notices; want nobody told",
				pushed, noticed, emails, inbox)
		}
	})

	t.Run("a task a person makes is told as ever", func(t *testing.T) {
		task := w.call(t, taskController.CreateTask, map[string]any{"task_project_uuid": w.launch, "task_name": "Venue contract"})["task_uuid"]
		if !within(20*time.Second, func() bool { return len(w.pushedTo("task", task)) > 0 }) {
			t.Fatal("Maya's new task was pushed to nobody")
		}
		if got, want := w.pushedTo("task", task), []string{"tok-ben", "tok-kay", "tok-sam"}; !slices.Equal(got, want) {
			t.Errorf("Maya's new task was pushed to %v, want %v", got, want)
		}
	})
}

// mark is where the broker's publishes are now, for heardOf.
func (b *noticeBroker) mark() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.got)
}

// heardOf is who, of people, was sent anything of id live since mark: an
// activity item (a comment, a mention) or the desktop app's notice.
func (b *noticeBroker) heardOf(id string, since int, people map[string]uuid.UUID) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var who []string
	for name, p := range people {
		topic := helpers.GetMqttTopicForUserActivity(p.String())
		for _, m := range b.got[since:] {
			var msg struct {
				Type int8 `json:"type"`
			}
			if m.topic == topic && bytes.Contains(m.payload, []byte(id)) && json.Unmarshal(m.payload, &msg) == nil &&
				(msg.Type == mqttStruct.MESSAGE_ACTIVITY || msg.Type == mqttStruct.MESSAGE_DESKTOP_NOTICE) {
				who = append(who, name)
			}
		}
	}
	slices.Sort(who)
	return who
}
