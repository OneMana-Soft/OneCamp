//go:build integration

package notification

// An email waits in the queue (quiet hours, retries, a busy worker), and the
// conversation goes on meanwhile. At the send the worker asks again what the
// live path asked at the event: the message is still there, the person is
// still in the place, their setting there still lets it through, and they
// haven't read it in the app since. An email that no longer holds is not
// sent, and its row says why (suppressed: no longer told).
//
// Run: go test -tags=integration ./business/Notification/ -run TestAnEmailStillHoldsWhenItIsSent -v

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	notifylevel "github.com/akashc777/OneCamp/business/NotifyLevel"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
)

func TestAnEmailStillHoldsWhenItIsSent(t *testing.T) {
	h := NewRecheck(t)
	ctx, dg := h.Ctx, h.DG
	type person = Person
	exec, newPerson, queued, send, rowSays, notTold := h.Exec, h.Person, h.Queued, h.Send, h.RowSays, h.NotTold
	// conversation starts a DM (two people) or a group (more, in the graph)
	// between people, each with the default setting, and with each but those
	// in unopened having opened it a minute ago; then from writes text there,
	// emailed to the rest.
	type written struct {
		grp string
		msg uuid.UUID
	}
	conversation := func(t *testing.T, from person, to []person, unopened map[uuid.UUID]bool, text string, mentioned bool) written {
		t.Helper()
		w := written{grp: helpers.GetGroupingId(from.ID.String(), to[0].ID.String()), msg: uuid.New()}
		everyone := append([]person{from}, to...)
		if len(to) > 1 {
			w.grp = "Grp-" + uuid.NewString()
			var in []map[string]any
			for _, p := range everyone {
				in = append(in, map[string]any{"uid": p.UID})
			}
			dg.Mutate(t, map[string]any{"uid": "_:g", "dm_grouping_id": w.grp, "dm_participants": in})
		}
		var ids []string
		for _, p := range everyone {
			exec(t, `INSERT INTO users_chat_notification (user_id, grp_id, notification_type) VALUES ($1, $2, 'all')`, p.ID, w.grp)
			if !unopened[p.ID] {
				exec(t, `INSERT INTO last_seen_chat (user_id, grp_id, user_last_seen) VALUES ($1, $2, NOW() - interval '1 minute')`, p.ID, w.grp)
			}
			if p.ID != from.ID {
				ids = append(ids, p.ID.String())
			}
		}
		exec(t, `INSERT INTO chats (id, created_by, grp_id) VALUES ($1, $2, $3)`, w.msg, from.ID, w.grp)
		DispatchChatDM(from.ID.String(), "Ada", "", w.grp, w.msg.String(), text, ids, mentioned)
		queued(t, to)
		return w
	}
	// channelPost starts a channel with from and to in it, each with the
	// default setting and having read it a minute ago; then from writes text
	// there, mentioning to, who are emailed it.
	channelPost := func(t *testing.T, from person, to []person, text string) written {
		t.Helper()
		w := written{grp: uuid.NewString(), msg: uuid.New()}
		// Channel names are unique: each test's its own.
		exec(t, `INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, false)`, w.grp, "launch-"+w.grp)
		var ids []string
		for _, p := range append([]person{from}, to...) {
			exec(t, `INSERT INTO users_channel_notification (user_id, channel_id, notification_type) VALUES ($1, $2, 'all')`, p.ID, w.grp)
			exec(t, `INSERT INTO last_seen_channel (user_id, channel_id, user_last_seen) VALUES ($1, $2, NOW() - interval '1 minute')`, p.ID, w.grp)
			if p.ID != from.ID {
				ids = append(ids, p.ID.String())
			}
		}
		exec(t, `INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, w.msg, w.grp, from.ID)
		DispatchChannelMention(from.ID.String(), "Ada", "", w.grp, "launch", w.msg.String(), text, ids)
		queued(t, to)
		return w
	}
	// reply answers w's message (a channel's post when inChannel) with text
	// from from, emailed to to, and answers the reply's id.
	reply := func(t *testing.T, w written, inChannel bool, from person, to []person, text string) uuid.UUID {
		t.Helper()
		c := uuid.New()
		exec(t, `INSERT INTO comments (id, created_by, created_at) VALUES ($1, $2, NOW())`, c, from.ID)
		var ids []string
		for _, p := range to {
			ids = append(ids, p.ID.String())
		}
		if inChannel {
			DispatchPostComment(from.ID.String(), "Ada", "", w.grp, w.msg.String(), text, c.String(), ids)
		} else {
			DispatchChatComment(from.ID.String(), "Ada", "", w.grp, w.msg.String(), text, c.String(), ids)
		}
		queued(t, to)
		return c
	}

	t.Run("a message that still holds is sent, opened or not", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		conversation(t, ada, []person{bo}, map[uuid.UUID]bool{bo.ID: true}, "the launch plan is ready", false)
		got := send(t)
		if len(got[bo.Email]) != 1 || !strings.Contains(got[bo.Email][0].Text, "the launch plan is ready") {
			t.Fatalf("Bo, who never opened the DM, was sent %v, want one email quoting the message", got[bo.Email])
		}
		if status, _ := rowSays(t, bo); status != "sent" {
			t.Errorf("Bo's row is %s, want sent", status)
		}
	})

	t.Run("a message deleted before the send is not quoted", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		w := conversation(t, ada, []person{bo}, nil, "the old salary sheet", false)
		exec(t, `UPDATE chats SET deleted_at = NOW() WHERE id = $1`, w.msg)
		notTold(t, send(t), bo, "the old salary sheet")
	})

	t.Run("someone who left the group before the send is sent nothing", func(t *testing.T) {
		ada, bo, cy := newPerson(t, "Ada"), newPerson(t, "Bo"), newPerson(t, "Cy")
		w := conversation(t, ada, []person{bo, cy}, nil, "the offsite budget", false)
		// Bo leaves as the graph has it (Leave's own step). The setting Bo
		// had there is kept: a setting can outlive a place (the start-up
		// repair takes it later), and who is in a group is the graph's.
		if err := chatDomain.LeaveGroup(ctx, w.grp, bo.UID); err != nil {
			t.Fatal(err)
		}
		got := send(t)
		notTold(t, got, bo, "nothing quotes this")
		if len(got[cy.Email]) != 1 {
			t.Errorf("Cy, still in the group, was sent %d emails, want 1", len(got[cy.Email]))
		}
	})

	t.Run("a DM read in the app before the send sends no email", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		w := conversation(t, ada, []person{bo}, nil, "are you around", false)
		exec(t, `UPDATE last_seen_chat SET user_last_seen = NOW() WHERE user_id = $1 AND grp_id = $2`, bo.ID, w.grp)
		notTold(t, send(t), bo, "are you around")
	})

	t.Run("a setting changed to Nothing before the send sends nothing, a mention included", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		w := conversation(t, ada, []person{bo}, nil, "@Bo the numbers", true)
		exec(t, `UPDATE users_chat_notification SET notification_type = 'block' WHERE user_id = $1 AND grp_id = $2`, bo.ID, w.grp)
		notTold(t, send(t), bo, "@Bo the numbers")
	})

	t.Run("a channel mention is asked again: who left, read it or set Nothing is sent nothing", func(t *testing.T) {
		ada, bo, cy, di, ed := newPerson(t, "Ada"), newPerson(t, "Bo"), newPerson(t, "Cy"), newPerson(t, "Di"), newPerson(t, "Ed")
		w := channelPost(t, ada, []person{bo, cy, di, ed}, "@all the launch moves to Friday")
		exec(t, `DELETE FROM users_channel_notification WHERE user_id = $1 AND channel_id = $2`, cy.ID, w.grp) // Cy leaves (Leave's own step)
		exec(t, `UPDATE last_seen_channel SET user_last_seen = NOW() WHERE user_id = $1 AND channel_id = $2`, di.ID, w.grp)
		exec(t, `UPDATE users_channel_notification SET notification_type = 'block' WHERE user_id = $1 AND channel_id = $2`, ed.ID, w.grp)
		got := send(t)
		if len(got[bo.Email]) != 1 || !strings.Contains(got[bo.Email][0].Text, "the launch moves to Friday") {
			t.Errorf("Bo, still in the channel and not caught up, was sent %v, want one email quoting the post", got[bo.Email])
		}
		for _, p := range []person{cy, di, ed} {
			if len(got[p.Email]) != 0 {
				t.Errorf("%s was sent %d emails, want none", p.Email, len(got[p.Email]))
			}
			if status, why := rowSays(t, p); status != "suppressed" || !strings.Contains(why, "no longer told") {
				t.Errorf("%s's row is %s (%q), want suppressed, no longer told", p.Email, status, why)
			}
		}
	})

	t.Run("a channel post deleted before the send is not quoted", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		w := channelPost(t, ada, []person{bo}, "the draft price list")
		exec(t, `UPDATE posts SET deleted_at = NOW() WHERE id = $1`, w.msg)
		notTold(t, send(t), bo, "the draft price list")
	})

	t.Run("a channel deleted before the send is not quoted", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		w := channelPost(t, ada, []person{bo}, "the reorg seating plan")
		exec(t, `UPDATE channels SET deleted_at = NOW() WHERE id = $1`, w.grp) // the channel's delete (SoftDeleteChannelInfo)
		notTold(t, send(t), bo, "the reorg seating plan")
	})

	t.Run("a check-in is asked again: who left the channel before the send is sent nothing", func(t *testing.T) {
		ada, bo, cy := newPerson(t, "Ada"), newPerson(t, "Bo"), newPerson(t, "Cy")
		w := channelPost(t, ada, []person{bo, cy}, "check-ins start on Monday")
		send(t) // the post's own emails
		DispatchCheckIn(uuid.NewString(), w.msg.String(), w.grp, "launch", "What did you ship this week?", []string{bo.ID.String(), cy.ID.String()})
		queued(t, []person{bo, cy})
		exec(t, `DELETE FROM users_channel_notification WHERE user_id = $1 AND channel_id = $2`, cy.ID, w.grp) // Cy leaves
		got := send(t)
		if len(got[bo.Email]) != 1 || !strings.Contains(got[bo.Email][0].Text, "What did you ship this week?") {
			t.Errorf("Bo, still in the channel, was sent %v, want the check-in's question", got[bo.Email])
		}
		notTold(t, got, cy, "nothing quotes this")
	})

	t.Run("a reply in a channel's thread is asked again: who left or read the thread is sent nothing", func(t *testing.T) {
		ada, bo, cy, di := newPerson(t, "Ada"), newPerson(t, "Bo"), newPerson(t, "Cy"), newPerson(t, "Di")
		w := channelPost(t, ada, []person{bo, cy, di}, "the launch checklist")
		send(t) // the post's own emails
		reply(t, w, true, ada, []person{bo, cy, di}, "the checklist has a new step")
		exec(t, `DELETE FROM users_channel_notification WHERE user_id = $1 AND channel_id = $2`, cy.ID, w.grp)
		// Di opens the thread in the app: its read marker (business/ThreadIndex: the thread's id is its post's).
		exec(t, `INSERT INTO threads (id, resource_kind, resource_uuid, anchor_kind, anchor_meta, created_by) VALUES ($1, 'channel', $2, 'message', '{}', $3)`, w.msg, w.grp, ada.ID)
		exec(t, `INSERT INTO thread_participants (thread_id, user_uuid, role, last_read_at) VALUES ($1, $2, 'reader', NOW())`, w.msg, di.ID)
		got := send(t)
		if len(got[bo.Email]) != 1 || !strings.Contains(got[bo.Email][0].Text, "the checklist has a new step") {
			t.Errorf("Bo, still in the channel and not caught up, was sent %v, want one email quoting the reply", got[bo.Email])
		}
		for _, p := range []person{cy, di} {
			if len(got[p.Email]) != 0 {
				t.Errorf("%s was sent %d emails, want none", p.Email, len(got[p.Email]))
			}
			if status, why := rowSays(t, p); status != "suppressed" || !strings.Contains(why, "no longer told") {
				t.Errorf("%s's row is %s (%q), want suppressed, no longer told", p.Email, status, why)
			}
		}
	})

	t.Run("a reply deleted before the send is not quoted", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		w := conversation(t, ada, []person{bo}, nil, "the venue shortlist", false)
		send(t)
		c := reply(t, w, false, ada, []person{bo}, "the second venue fell through")
		exec(t, `UPDATE comments SET deleted_at = NOW() WHERE id = $1`, c)
		notTold(t, send(t), bo, "the second venue fell through")
	})

	t.Run("a reply under a post deleted before the send is not quoted", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		w := channelPost(t, ada, []person{bo}, "the old roadmap")
		send(t)
		reply(t, w, true, ada, []person{bo}, "the roadmap's third item")
		exec(t, `UPDATE posts SET deleted_at = NOW() WHERE id = $1`, w.msg)
		notTold(t, send(t), bo, "the roadmap's third item")
	})

	// taskWork starts a project with from and to in it (from its admin), each
	// with the default setting, and a task there; then from comments text on
	// the task, emailed to to.
	type work struct {
		project, task   string // in the graph
		taskID, comment uuid.UUID
	}
	taskWork := func(t *testing.T, from person, to []person, text string) work {
		t.Helper()
		projectID, w := uuid.New(), work{taskID: uuid.New(), comment: uuid.New()}
		exec(t, `INSERT INTO projects (id, project_name, created_by) VALUES ($1, $2, $3)`, projectID, "Launch-"+projectID.String(), from.ID)
		exec(t, `INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, w.taskID, projectID, from.ID)
		var members []map[string]any
		var ids []string
		for _, p := range append([]person{from}, to...) {
			members = append(members, map[string]any{"uid": p.UID})
			exec(t, `INSERT INTO users_project_notification (user_id, project_id, notification_type) VALUES ($1, $2, 'all')`, p.ID, projectID)
			if p.ID != from.ID {
				ids = append(ids, p.ID.String())
			}
		}
		live := "0001-01-01T00:00:00Z"
		uids := dg.Mutate(t, map[string]any{"uid": "_:p", "dgraph.type": "Project", "project_uuid": projectID.String(), "project_name": "Launch", "project_deleted_at": live,
			"project_members": members, "project_admins": []map[string]any{{"uid": from.UID}},
			"project_tasks": []map[string]any{{"uid": "_:t", "dgraph.type": "Task", "task_uuid": w.taskID.String(), "task_name": "Ship the FAQ",
				"task_status": "todo", "task_deleted_at": live, "task_project": map[string]any{"uid": "_:p"}, "task_created_by": map[string]any{"uid": from.UID}}}})
		w.project, w.task = uids["p"], uids["t"]
		exec(t, `INSERT INTO comments (id, created_by, created_at) VALUES ($1, $2, NOW())`, w.comment, from.ID)
		DispatchTaskComment(from.ID.String(), "Ada", "", w.taskID.String(), "Ship the FAQ", text, w.comment.String(), ids)
		queued(t, to)
		return w
	}

	t.Run("a task's email is asked again: who was taken off its project or set Nothing is sent nothing", func(t *testing.T) {
		ada, bo, cy, di := newPerson(t, "Ada"), newPerson(t, "Bo"), newPerson(t, "Cy"), newPerson(t, "Di")
		w := taskWork(t, ada, []person{bo, cy, di}, "the FAQ needs a pricing answer")
		if err := projectDomain.RemoveMemberFromProject(ctx, w.project, cy.UID); err != nil {
			t.Fatal(err)
		}
		exec(t, `UPDATE users_project_notification SET notification_type = 'block' WHERE user_id = $1`, di.ID)
		got := send(t)
		if len(got[bo.Email]) != 1 || !strings.Contains(got[bo.Email][0].Text, "the FAQ needs a pricing answer") {
			t.Errorf("Bo, still in the project, was sent %v, want one email quoting the comment", got[bo.Email])
		}
		notTold(t, got, cy, "nothing quotes this")
		notTold(t, got, di, "nothing quotes this")
	})

	t.Run("a task deleted before the send is not quoted", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		w := taskWork(t, ada, []person{bo}, "the FAQ's last section")
		dg.Mutate(t, map[string]any{"uid": w.task, "task_deleted_at": time.Now().UTC().Format(time.RFC3339)})
		notTold(t, send(t), bo, "the FAQ's last section")
	})

	t.Run("a task comment deleted before the send is not quoted", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		w := taskWork(t, ada, []person{bo}, "the FAQ's draft link")
		exec(t, `UPDATE comments SET deleted_at = NOW() WHERE id = $1`, w.comment)
		notTold(t, send(t), bo, "the FAQ's draft link")
	})

	t.Run("an assignment set to Nothing before the send sends nothing", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		w := taskWork(t, ada, []person{bo}, "the FAQ is taking shape")
		send(t) // the comment's own email
		DispatchTaskAssignment(ada.ID.String(), "Ada", "", w.taskID.String(), "Ship the FAQ", "Launch", bo.ID.String())
		queued(t, []person{bo})
		exec(t, `UPDATE users_project_notification SET notification_type = 'block' WHERE user_id = $1`, bo.ID)
		notTold(t, send(t), bo, "nothing quotes this")
	})

	// decides counts, from here on, the task read decisions (taskread's
	// Decide) made, each made as the installed rule makes it.
	decides := func(t *testing.T) *atomic.Int32 {
		t.Helper()
		was, rule := taskread.Decide, taskread.Decider()
		t.Cleanup(func() { taskread.Decide = was })
		var n atomic.Int32
		taskread.Decide = func(ctx context.Context, reader string) (map[string]bool, error) {
			n.Add(1)
			return rule(ctx, reader)
		}
		return &n
	}

	t.Run("tasks restricted before the send: a member they leave out is sent nothing, their assignees and a granted person are", func(t *testing.T) {
		ada, bo, cy, di, ed := newPerson(t, "Ada"), newPerson(t, "Bo"), newPerson(t, "Cy"), newPerson(t, "Di"), newPerson(t, "Ed")
		faq, docs := taskWork(t, ada, []person{bo, cy, di}, "the FAQ's pricing answer"), taskWork(t, ada, []person{ed}, "the docs' pricing page")
		// Cy is in the docs' project too, and is told of its comment.
		dg.Mutate(t, map[string]any{"uid": docs.project, "project_members": []map[string]any{{"uid": cy.UID}}})
		exec(t, `INSERT INTO users_project_notification (user_id, project_id, notification_type) SELECT $1, project_id, 'all' FROM tasks WHERE id = $2`, cy.ID, docs.taskID)
		DispatchTaskComment(ada.ID.String(), "Ada", "", docs.taskID.String(), "Ship the FAQ", "the docs' pricing page", docs.comment.String(), []string{cy.ID.String()})
		queued(t, nil)
		dg.Mutate(t, map[string]any{"uid": faq.task, "task_assignee": map[string]any{"uid": bo.UID}})
		dg.Mutate(t, map[string]any{"uid": docs.task, "task_assignee": map[string]any{"uid": ed.UID}})
		for _, w := range []work{faq, docs} {
			exec(t, `INSERT INTO task_restrictions (task_id, project_id, restricted_by) SELECT id, project_id, $2 FROM tasks WHERE id = $1`, w.taskID, ada.ID)
		}
		exec(t, `INSERT INTO task_restriction_grants (task_id, user_id, granted_by) VALUES ($1, $2, $3)`, faq.taskID, di.ID, ada.ID)
		n := decides(t)
		got := send(t)
		for p, text := range map[person]string{bo: "the FAQ's pricing answer", di: "the FAQ's pricing answer", ed: "the docs' pricing page"} {
			if len(got[p.Email]) != 1 || !strings.Contains(got[p.Email][0].Text, text) {
				t.Errorf("%s, who still reads the task, was sent %v, want one email quoting %q", p.Email, got[p.Email], text)
			}
		}
		if len(got[cy.Email]) != 0 {
			t.Errorf("Cy, whom both tasks leave out, was sent %d emails, want none", len(got[cy.Email]))
		}
		if status, why := rowSays(t, cy); status != "suppressed" || !strings.Contains(why, "no longer told") {
			t.Errorf("Cy's row is %s (%q), want suppressed, no longer told", status, why)
		}
		// Asked once for the one person left out, across both tasks.
		if got := n.Load(); got != 1 {
			t.Errorf("the batch decided %d times, want once (Cy, for both tasks)", got)
		}
	})

	// urgent tells to, as business/Task's Urgent digest does, that by made
	// each of ws's tasks Urgent: one email naming them all.
	urgent := func(t *testing.T, by, to person, ws ...work) {
		t.Helper()
		var ids, names []string
		for _, w := range ws {
			ids, names = append(ids, w.taskID.String()), append(names, "Ship the FAQ")
		}
		Tell(ctx, Notice{
			Kind: "task.urgent_digest", ResourceKind: "task", ResourceUUID: ids[0],
			Actor: &dgraphStruct.DgraphUser{Uuid: by.ID.String(), UserName: "Ada"}, DedupKey: "task.urgent_digest|" + uuid.NewString(),
			Title: "Ada marked " + strconv.Itoa(len(ws)) + " tasks Urgent", Body: strings.Join(names, ", ") + " before the launch review",
			Path: "/app/inbox", Email: EventTaskUrgent,
			About: &About{Kind: AboutTask, Item: ids[0], Items: ids, Heard: notifylevel.Named},
		}, []NoticeRecipient{{UserUUID: to.ID.String(), Reason: "assignee"}})
		queued(t, []person{to})
	}

	// twoTasks is two tasks of from's, each in its own project with to in it,
	// their comments' own emails sent.
	twoTasks := func(t *testing.T, from, to person) (work, work) {
		t.Helper()
		faq := taskWork(t, from, []person{to}, "the FAQ's owner")
		send(t)
		docs := taskWork(t, from, []person{to}, "the docs' owner")
		send(t)
		return faq, docs
	}

	t.Run("an Urgent digest whose every task still holds is sent", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		faq, docs := twoTasks(t, ada, bo)
		urgent(t, ada, bo, faq, docs)
		n := decides(t)
		if got := send(t); len(got[bo.Email]) != 1 || !strings.Contains(got[bo.Email][0].Text, "before the launch review") {
			t.Errorf("Bo, who still reads both tasks, was sent %v, want the digest", got[bo.Email])
		}
		if got := n.Load(); got != 0 {
			t.Errorf("a batch with no restricted task decided %d times, want none", got)
		}
	})

	t.Run("an Urgent digest with one task restricted from its reader before the send sends nothing", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		faq, docs := twoTasks(t, ada, bo)
		urgent(t, ada, bo, faq, docs)
		exec(t, `INSERT INTO task_restrictions (task_id, project_id, restricted_by) SELECT id, project_id, $2 FROM tasks WHERE id = $1`, docs.taskID, ada.ID)
		notTold(t, send(t), bo, "before the launch review")
	})

	t.Run("an Urgent digest with one task deleted before the send sends nothing", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		faq, docs := twoTasks(t, ada, bo)
		urgent(t, ada, bo, faq, docs)
		dg.Mutate(t, map[string]any{"uid": docs.task, "task_deleted_at": time.Now().UTC().Format(time.RFC3339)})
		notTold(t, send(t), bo, "before the launch review")
	})

	// projectUpdate starts a project with from (its admin) and to in it, each
	// with the default setting; then from posts an update there saying text,
	// emailed to to.
	type posted struct {
		project           string // in the graph
		projectID, update uuid.UUID
	}
	projectUpdate := func(t *testing.T, from person, to []person, text string) posted {
		t.Helper()
		u := posted{projectID: uuid.New(), update: uuid.New()}
		exec(t, `INSERT INTO projects (id, project_name, created_by) VALUES ($1, $2, $3)`, u.projectID, "Launch-"+u.projectID.String(), from.ID)
		var members []map[string]any
		var ids []string
		for _, p := range append([]person{from}, to...) {
			members = append(members, map[string]any{"uid": p.UID})
			exec(t, `INSERT INTO users_project_notification (user_id, project_id, notification_type) VALUES ($1, $2, 'all')`, p.ID, u.projectID)
			ids = append(ids, p.ID.String())
		}
		u.project = dg.Mutate(t, map[string]any{"uid": "_:p", "dgraph.type": "Project", "project_uuid": u.projectID.String(), "project_name": "Launch",
			"project_deleted_at": "0001-01-01T00:00:00Z", "project_members": members, "project_admins": []map[string]any{{"uid": from.UID}}})["p"]
		exec(t, `INSERT INTO project_updates (id, project_uuid, author_uuid, health, body) VALUES ($1, $2, $3, 'on_track', $4)`, u.update, u.projectID, from.ID, text)
		DispatchProjectUpdate(from.ID.String(), "Ada", u.projectID.String(), "Launch", "On track", text, u.update.String(), ids)
		queued(t, to)
		return u
	}

	t.Run("a project's update is asked again: who was taken off it or set Mentions is sent nothing", func(t *testing.T) {
		ada, bo, cy, di := newPerson(t, "Ada"), newPerson(t, "Bo"), newPerson(t, "Cy"), newPerson(t, "Di")
		u := projectUpdate(t, ada, []person{bo, cy, di}, "the launch slips a week")
		if err := projectDomain.RemoveMemberFromProject(ctx, u.project, cy.UID); err != nil {
			t.Fatal(err)
		}
		exec(t, `UPDATE users_project_notification SET notification_type = 'mention' WHERE user_id = $1`, di.ID)
		got := send(t)
		if len(got[bo.Email]) != 1 || !strings.Contains(got[bo.Email][0].Text, "the launch slips a week") {
			t.Errorf("Bo, still in the project, was sent %v, want one email quoting the update", got[bo.Email])
		}
		notTold(t, got, cy, "nothing quotes this")
		notTold(t, got, di, "nothing quotes this")
	})

	t.Run("a project update deleted before the send is not quoted", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		u := projectUpdate(t, ada, []person{bo}, "the launch date moved")
		exec(t, `UPDATE project_updates SET deleted_at = NOW() WHERE id = $1`, u.update)
		notTold(t, send(t), bo, "the launch date moved")
	})

	t.Run("a project deleted before the send is not quoted", func(t *testing.T) {
		ada, bo := newPerson(t, "Ada"), newPerson(t, "Bo")
		u := projectUpdate(t, ada, []person{bo}, "the launch is cancelled")
		// Archived as the app does (business/Project ArchiveProjectByProjectUUID):
		// Postgres first, then the graph.
		exec(t, `UPDATE projects SET deleted_at = NOW(), updated_at = NOW() WHERE id = $1`, u.projectID)
		dg.Mutate(t, map[string]any{"uid": u.project, "project_deleted_at": time.Now().UTC().Format(time.RFC3339)})
		notTold(t, send(t), bo, "the launch is cancelled")
	})
}
