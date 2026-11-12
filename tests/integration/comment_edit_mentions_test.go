//go:build integration
// +build integration

package integration_test

// A comment's edit tells whom it adds, by name, and never someone who may not
// hear of it (NOTIF-H3, CR-B25 F1-F3). In each place a comment is written (a
// task, a reply in #launch, a reply in a group, a doc), Maya edits her comment
// to name Ben and Cy. Ben, who may read it, is told once: the push and the
// email say who named him, and where. Cy is in none of those places (not in
// #launch or the group, off the project though her setting for it is left
// behind, not a reader of the doc), so she is told nothing; a task comment
// may not name her at all, so an edit of the task's description does. A
// reply's notice names its message by the keys the web opens it by (post_id,
// message_id).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestACommentsEditTellsWhomItAddsByNameAndNoOutsider(t *testing.T) {
	w, channel := newEditWorld(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	cy := uuid.New()
	w.people["cy"] = cy
	exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, 'cy@acme.test', 'cy', 'cy', NOW(), NOW())`, cy)
	exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, 'unsub-cy')`, cy)
	exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-cy')`, cy)
	project, task, doc := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Spring launch', $2)`, project, w.people["maya"])
	exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, task, project, w.people["maya"])
	for name, level := range map[string]string{"maya": "all", "ada": "mention", "ben": "mention", "cy": "all"} {
		exec(`INSERT INTO users_project_notification (user_id, project_id, notification_type) VALUES ($1, $2, $3)`, w.people[name], project, level)
	}
	refs := func(names ...string) []map[string]any {
		var out []map[string]any
		for _, n := range names {
			out = append(out, map[string]any{"uid": w.uids[n]})
		}
		return out
	}
	got := w.dg.Mutate(t, []map[string]any{
		{"uid": "_:cy", "dgraph.type": "User", "user_uuid": cy.String(), "user_name": "cy", "user_deleted_at": liveAt},
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": uuid.NewString(), "team_name": "Launch",
			"team_members": refs("maya", "ada", "ben"), "team_admins": refs("maya")},
		{"uid": "_:project", "dgraph.type": "Project", "project_uuid": project, "project_name": "Spring launch", "project_deleted_at": liveAt,
			"project_team": map[string]any{"uid": "_:team"}, "project_members": refs("maya", "ada", "ben"), "project_admins": refs("maya"),
			"project_created_by": refs("maya")[0]},
		{"uid": "_:task", "dgraph.type": "Task", "task_uuid": task, "task_name": "Venue contract", "task_status": "todo", "task_deleted_at": liveAt,
			"task_project": map[string]any{"uid": "_:project"}, "task_created_by": refs("maya")[0]},
		{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "Launch plan", "doc_private": true, "doc_public_comment": true,
			"doc_deleted_at": liveAt, "doc_created_by": refs("maya")[0], "doc_editing_users": refs("maya"), "doc_reading_users": refs("ada", "ben")},
	})
	w.uids["cy"] = got["cy"]
	mayaInfo := w.as(t, "maya").Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	maya := mayaInfo.UserDgraphInfo.DisplayName()
	if maya == "" {
		t.Fatal("Maya has no name to be told by")
	}

	// do runs handler as Maya and answers the comment it made, if any.
	do := func(handler http.HandlerFunc, body any) string {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw))).WithContext(w.as(t, "maya")))
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		var made struct {
			ID string `json:"comment_id"`
		}
		_ = json.Unmarshal(out.Data, &made)
		return made.ID
	}
	post := w.send(t, "maya", postController.CreatePost, http.MethodPost,
		postAdapter.InputCreateOrUpdatePostInfo{ChannelUuid: channel.String(), HTMLText: "<p>Venue is booked</p>"})
	chat := w.send(t, "maya", chatController.CreateGroupChat, http.MethodPost, chatAdapter.ChatInfo{
		Participants: []string{w.uids["maya"], w.uids["ada"], w.uids["ben"]}, TextHtml: "<p>Our plan</p>"})

	// pushed is the titles of the pushes about thread that reached name's
	// device after the first `since` pushes.
	pushed := func(since int, name, thread string) []string {
		w.mu.Lock()
		defer w.mu.Unlock()
		var titles []string
		for _, p := range w.pushes[since:] {
			if thread != "" && p.data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] != thread {
				continue
			}
			for _, tok := range p.tokens {
				if tok == "tok-"+name {
					titles = append(titles, p.data[firebaseInit.FIREBASE_PUSH_DATA_TITLE])
				}
			}
		}
		return titles
	}
	// emailed is the subjects of the emails about the comment queued for name.
	emailed := func(name, comment string) []string {
		t.Helper()
		rows, err := w.env.PG.Query(`SELECT subject FROM notification_email_queue WHERE user_id = $1 AND strpos(dedup_key, $2) > 0`, w.people[name], comment)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var subjects []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			subjects = append(subjects, s)
		}
		return subjects
	}
	// heard is everything name was ever told: pushes, emails, Inbox notices.
	heard := func(name string) (pushes, emails, inbox int) {
		t.Helper()
		pushes = len(pushed(0, name, ""))
		if err := w.env.PG.QueryRow(`SELECT (SELECT count(*) FROM notification_email_queue WHERE user_id = $1),
			(SELECT count(*) FROM resource_notices WHERE user_uuid = $1)`, w.people[name]).Scan(&emails, &inbox); err != nil {
			t.Fatal(err)
		}
		return pushes, emails, inbox
	}
	// notices is the data of each Inbox notice about the comment name holds.
	notices := func(name, comment string) []map[string]string {
		t.Helper()
		rows, err := w.env.PG.Query(`SELECT data FROM resource_notices WHERE user_uuid = $1 AND data->>'comment_uuid' = $2`, w.people[name], comment)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []map[string]string
		for rows.Next() {
			var raw []byte
			d := map[string]string{}
			if err := rows.Scan(&raw); err != nil || json.Unmarshal(raw, &d) != nil {
				t.Fatalf("a notice's data: %v %s", err, raw)
			}
			out = append(out, d)
		}
		return out
	}

	for _, p := range []struct {
		what, thread, title string
		names               map[string]string // what a notice's data names: key -> id
	}{
		{what: "a task comment", thread: task, title: maya + " mentioned you in Venue contract"},
		{what: "a reply in #launch", thread: post, title: maya + " mentioned you in #launch", names: map[string]string{"post_id": post}},
		{what: "a reply in the group", thread: chat, title: maya + " mentioned you in a conversation", names: map[string]string{"message_id": chat}},
		{what: "a comment on the doc", title: maya + " mentioned you in a comment on Launch plan"},
	} {
		var comment string
		var edit func(html string)
		// The edit names Ben and Cy. A task comment may name only someone who
		// can see the task (MentionRefusal), so there Cy is named by an edit
		// of the task's description, the task's other path to its Hear.
		named := " " + w.chip("ben") + " " + w.chip("cy") + "</p>"
		outsider := func() {}
		switch p.what {
		case "a task comment":
			comment = do(taskController.CreateTaskComment, map[string]any{"task_uuid": task, "task_comment_body": "<p>Signed</p>"})
			edit = func(html string) {
				do(taskController.UpdateTaskCommentBody, map[string]any{"task_comment_uuid": comment, "task_uuid": task, "task_comment_body": html})
			}
			named = " " + w.chip("ben") + "</p>"
			outsider = func() {
				do(taskController.UpdateTaskDesc, map[string]any{"task_uuid": task, "task_project_uuid": project, "task_description": "<p>Terms " + w.chip("cy") + "</p>"})
			}
		case "a reply in #launch":
			comment = do(postController.CreateCommentInPost, map[string]any{"post_id": post, "comment_text_html": "<p>Signed</p>"})
			edit = func(html string) {
				do(postController.UpdateCommentInPost, map[string]any{"post_id": post, "comment_id": comment, "comment_text_html": html})
			}
		case "a reply in the group":
			comment = do(chatController.CreateChatComment, map[string]any{"chat_id": chat, "comment_text_html": "<p>Signed</p>"})
			edit = func(html string) {
				do(chatController.UpdateChatComment, map[string]any{"chat_id": chat, "comment_id": comment, "comment_text_html": html})
			}
		case "a comment on the doc":
			comment = do(docController.CreateDocComment, map[string]any{"doc_uuid": doc, "doc_comment_body": "<p>Signed</p>"})
			edit = func(html string) {
				do(docController.UpdateDocComment, map[string]any{"doc_comment_uuid": comment, "doc_uuid": doc, "doc_comment_body": html})
			}
		}
		if comment == "" {
			t.Fatalf("%s: no comment made", p.what)
		}
		helpers.SettleBackground(10 * time.Second)
		w.mu.Lock()
		since := len(w.pushes)
		w.mu.Unlock()
		cyPushes, cyEmails, cyInbox := heard("cy")

		edit("<p>Signed" + named)
		outsider()
		if !within(20*time.Second, func() bool { return len(emailed("ben", comment)) > 0 }) {
			t.Errorf("%s: Ben, whom its edit names, wasn't emailed", p.what)
		}
		helpers.SettleBackground(10 * time.Second)

		// Ben is told once, by name: the push (none for a doc) and the email.
		if p.thread != "" {
			if got := pushed(since, "ben", p.thread); len(got) != 1 || got[0] != p.title {
				t.Errorf("%s: the edit pushed Ben %q; want once, %q", p.what, got, p.title)
			}
		}
		if got := emailed("ben", comment); len(got) != 1 || !strings.Contains(got[0], p.title) {
			t.Errorf("%s: the edit emailed Ben %q; want once, saying %q", p.what, got, p.title)
		}
		if got := notices("ben", comment); len(got) != 1 {
			t.Errorf("%s: Ben holds %d notices of the edit; want one", p.what, len(got))
		} else {
			for key, id := range p.names {
				if got[0][key] != id {
					t.Errorf("%s: Ben's notice has %s %q, want %q, the message the web opens (its data: %v)", p.what, key, got[0][key], id, got[0])
				}
			}
		}
		// Cy, who may not hear of it, is told nothing.
		if pushes, emails, inbox := heard("cy"); pushes != cyPushes || emails != cyEmails || inbox != cyInbox {
			t.Errorf("%s: Cy, who may not hear of it, was told of the edit naming her: %d pushes, %d emails, %d Inbox notices; want none",
				p.what, pushes-cyPushes, emails-cyEmails, inbox-cyInbox)
		}
	}
}
