//go:build integration
// +build integration

package integration_test

// A member's setting for a channel, a DM or a project means one thing on
// every path (NOTIF M1): "Nothing" tells them nothing, an @mention of them
// included; "Mentions" tells them only what names them or a thread they are
// in; "Everything" tells them everything. Each test drives a path that once
// told a muted member anyway, beside someone on "Mentions" who is told.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestMuted' -v

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	notification "github.com/akashc777/OneCamp/business/Notification"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	activityDomain "github.com/akashc777/OneCamp/domain/Activity"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// pushesTo is every push that reached name's device, about anything.
func (w *editWorld) pushesTo(name string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, p := range w.pushes {
		for _, tok := range p.tokens {
			if tok == "tok-"+name {
				n++
			}
		}
	}
	return n
}

// emailsTo is every email queued for name, about anything.
func (w *editWorld) emailsTo(t *testing.T, name string) int {
	t.Helper()
	var n int
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM notification_email_queue WHERE user_id = $1`, w.people[name]).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (w *editWorld) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if res, err := w.env.PG.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("%s: %d rows", q, n)
	}
}

// (a) Ada wrote a message in #launch and then set #launch to "Nothing". Maya
// answers it in its thread and names Ada and Ben there. Ben ("Mentions") is
// pushed; Ada is pushed and emailed nothing, for her own message's thread or
// for being named in it.
func TestMutedChannelsThreadRepliesTellNothing(t *testing.T) {
	w, channel := newEditWorld(t)
	post := w.send(t, "ada", postController.CreatePost, http.MethodPost,
		postAdapter.InputCreateOrUpdatePostInfo{ChannelUuid: channel.String(), HTMLText: "<p>Venue options attached</p>"})
	helpers.SettleBackground(10 * time.Second)
	w.exec(t, `UPDATE users_channel_notification SET notification_type = 'block' WHERE user_id = $1 AND channel_id = $2`, w.people["ada"], channel)
	adaPushes, adaEmails, benPushes := w.pushesTo("ada"), w.emailsTo(t, "ada"), w.pushesTo("ben")

	w.send(t, "maya", postController.CreateCommentInPost, http.MethodPost, postAdapter.InputCreateOrUpdateCommentToPost{
		PostUuid: post, HTMLText: "<p>Second one " + w.chip("ada") + " " + w.chip("ben") + "</p>"})
	if !within(20*time.Second, func() bool { return w.pushesTo("ben") > benPushes }) {
		t.Fatal("Ben (Mentions), named in the reply, wasn't pushed")
	}
	helpers.SettleBackground(10 * time.Second)
	if got := w.pushesTo("ada") - adaPushes; got != 0 {
		t.Errorf("Ada set #launch to Nothing, yet the thread reply naming her pushed her %d times", got)
	}
	if got := w.emailsTo(t, "ada") - adaEmails; got != 0 {
		t.Errorf("Ada set #launch to Nothing, yet the thread reply naming her emailed her %d times", got)
	}
}

// (b) Ada set #launch to "Nothing". Maya names Ada and Ben in one message
// and writes @channel in another. Ada's Inbox neither lists nor counts
// either: the badge and the list agree, and the messages stay in #launch for
// her to read. Ben ("Mentions") has both listed, as he is pushed for both.
func TestMutedChannelsMentionsAreNeitherListedNorCounted(t *testing.T) {
	w, channel := newEditWorld(t)
	w.exec(t, `UPDATE users_channel_notification SET notification_type = 'block' WHERE user_id = $1 AND channel_id = $2`, w.people["ada"], channel)
	for _, name := range []string{"ada", "ben"} {
		if err := activityBusiness.MarkActivitySeen(context.Background(), w.people[name]); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	benPushes := w.pushesTo("ben")

	named := w.send(t, "maya", postController.CreatePost, http.MethodPost,
		postAdapter.InputCreateOrUpdatePostInfo{ChannelUuid: channel.String(), HTMLText: "<p>Agenda " + w.chip("ada") + " " + w.chip("ben") + "</p>"})
	everyone := w.send(t, "maya", postController.CreatePost, http.MethodPost,
		postAdapter.InputCreateOrUpdatePostInfo{ChannelUuid: channel.String(), HTMLText: "<p>" + chip("broadcast:channel", "channel") + " doors open at 9</p>"})
	if !within(20*time.Second, func() bool { return w.pushesTo("ben") >= benPushes+2 }) {
		t.Fatal("Ben (Mentions) wasn't pushed for the mention and the @channel")
	}
	helpers.SettleBackground(10 * time.Second)

	for what, id := range map[string]string{"the message naming her": named, "the @channel": everyone} {
		if items, _ := inboxAbout(t, w.people["ada"], w.uids["ada"], id); len(items) != 0 {
			t.Errorf("Ada set #launch to Nothing, yet her Inbox lists %d items about %s", len(items), what)
		}
		if items, _ := inboxAbout(t, w.people["ben"], w.uids["ben"], id); len(items) == 0 {
			t.Errorf("Ben (Mentions) was pushed for %s, yet his Inbox doesn't list it", what)
		}
	}
	if _, badge := inboxAbout(t, w.people["ada"], w.uids["ada"], named); badge != 0 {
		t.Errorf("Ada set #launch to Nothing, yet her Inbox badge says %d", badge)
	}
	if _, badge := inboxAbout(t, w.people["ben"], w.uids["ben"], named); badge < 2 {
		t.Errorf("Ben (Mentions) has a badge of %d, want the mention and the @channel", badge)
	}
}

// (b2) The Inbox reads a page's held notices and the levels of its own places
// in ONE statement on the request's ctx (CR-398 F2; NOTIF M1 store budget):
// Ada's #launch and her group, by lower-case id; a channel and a group she
// has no setting for are in neither; Ben's settings are not Ada's. A notice is
// held by its key, a long one by its stored key, a retracted one too; a listed
// one isn't. A read of keys alone answers no level, and a read of places alone
// holds no notice.
func TestTheInboxReadsThePagesOwnLevels(t *testing.T) {
	w, channel := newEditWorld(t)
	ada := w.people["ada"]
	w.exec(t, `UPDATE users_channel_notification SET notification_type = 'block' WHERE user_id = $1 AND channel_id = $2`, ada, channel)
	w.exec(t, `INSERT INTO users_chat_notification (user_id, grp_id, notification_type) VALUES ($1, 'Grp-Launch', 'mention')`, ada)
	w.exec(t, `INSERT INTO users_chat_notification (user_id, grp_id, notification_type) VALUES ($1, 'Grp-Launch', 'block')`, w.people["ben"])
	told := followModels.MentionKey("post", uuid.NewString()) + "|" + ada.String()
	long := followModels.MentionKey("chat", strings.Repeat("x", 300)) + "|" + ada.String()
	listed := followModels.MentionKey("comment", uuid.NewString()) + "|" + ada.String()
	for key, reason := range map[string]string{told: "mention", long: followModels.ReasonRetracted, listed: followModels.ReasonListed} {
		stored, _ := followModels.StoredKeys([]string{key})
		w.exec(t, `INSERT INTO resource_notices (id, user_uuid, kind, resource_kind, resource_uuid, reason, dedup_key) VALUES ($1, $2, 'mention', 'post', $3, $4, $5)`,
			uuid.New(), ada, uuid.NewString(), reason, stored[0])
	}
	keys := []string{told, long, listed, followModels.MentionKey("post", uuid.NewString()) + "|" + ada.String()}
	places, grps := []string{strings.ToUpper(channel.String()), uuid.NewString()}, []string{"Grp-Launch", "grp-other"}
	ctx := context.Background()
	s, err := activityDomain.ReaderPageSettings(ctx, ada.String(), keys, places, grps)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{told: true, long: true}; !reflect.DeepEqual(s.Held, want) {
		t.Errorf("Ada's held notices read %v, want %v", s.Held, want)
	}
	if want := map[string]string{channel.String(): "block"}; !reflect.DeepEqual(s.ByChannel, want) {
		t.Errorf("Ada's channels read %v, want %v", s.ByChannel, want)
	}
	if want := map[string]string{"grp-launch": "mention"}; !reflect.DeepEqual(s.ByChat, want) {
		t.Errorf("Ada's conversations read %v, want %v", s.ByChat, want)
	}
	if s, err := activityDomain.ReaderPageSettings(ctx, ada.String(), keys, nil, nil); err != nil || len(s.Held) != 2 || len(s.ByChannel)+len(s.ByChat) != 0 {
		t.Errorf("keys alone read %+v %v, want the two held notices and no level", s, err)
	}
	if s, err := activityDomain.ReaderPageSettings(ctx, ada.String(), nil, places, grps); err != nil || len(s.Held) != 0 || len(s.ByChannel) != 1 || len(s.ByChat) != 1 {
		t.Errorf("places alone read %+v %v, want Ada's two levels and no notice", s, err)
	}
	if s, err := activityDomain.ReaderPageSettings(ctx, ada.String(), nil, nil, nil); err != nil || len(s.Held)+len(s.ByChannel)+len(s.ByChat) != 0 {
		t.Errorf("a page with no key and no place read %+v %v, want nothing", s, err)
	}
}

// (c) Ada set the Spring launch project to "Nothing"; Ben to "Mentions". A
// task comment names both, the task is given to each, its status changes for
// both, and the project posts an update. Ben is emailed what names him (the
// comment, his assignment, the status of his task) and not the update; Ada is
// emailed nothing.
func TestMutedProjectEmailsNothing(t *testing.T) {
	w, _ := newEditWorld(t)
	project, task := uuid.NewString(), uuid.NewString()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Spring launch', $2)`, project, w.people["maya"])
	exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, task, project, w.people["maya"])
	for name, level := range map[string]string{"maya": "all", "ada": "block", "ben": "mention"} {
		exec(`INSERT INTO users_project_notification (user_id, project_id, notification_type) VALUES ($1, $2, $3)`, w.people[name], project, level)
	}
	refs := func(names ...string) []map[string]any {
		var out []map[string]any
		for _, n := range names {
			out = append(out, map[string]any{"uid": w.uids[n]})
		}
		return out
	}
	w.dg.Mutate(t, []map[string]any{
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": uuid.NewString(), "team_name": "Launch",
			"team_members": refs("maya", "ada", "ben"), "team_admins": refs("maya")},
		{"uid": "_:project", "dgraph.type": "Project", "project_uuid": project, "project_name": "Spring launch", "project_deleted_at": liveAt,
			"project_team": map[string]any{"uid": "_:team"}, "project_members": refs("maya", "ada", "ben"), "project_admins": refs("maya"),
			"project_created_by": refs("maya")[0]},
		{"uid": "_:task", "dgraph.type": "Task", "task_uuid": task, "task_name": "Venue contract", "task_status": "todo", "task_deleted_at": liveAt,
			"task_project": map[string]any{"uid": "_:project"}, "task_created_by": refs("maya")[0]},
	})
	maya := w.people["maya"].String()
	steps := []struct {
		what    string
		do      func()
		benHear bool
	}{
		{"a task comment naming them", func() {
			w.send(t, "maya", taskController.CreateTaskComment, http.MethodPost, map[string]any{"task_uuid": task,
				"task_comment_body": "<p>Signed? " + w.chip("ada") + " " + w.chip("ben") + "</p>"})
		}, true},
		{"the task given to them", func() {
			notification.DispatchTaskAssignment(maya, "maya", "", task, "Venue contract", "Spring launch", w.people["ada"].String())
			notification.DispatchTaskAssignment(maya, "maya", "", task, "Venue contract", "Spring launch", w.people["ben"].String())
		}, true},
		{"their task's status", func() {
			notification.DispatchTaskStatusChange(maya, "maya", nil, task, "Venue contract", "Spring launch", "To do", "Done",
				uuid.NewString(), []string{w.people["ada"].String(), w.people["ben"].String()})
		}, true},
		{"the project's update", func() {
			notification.DispatchProjectUpdate(maya, "maya", project, "Spring launch", "On track", "Venue signed", uuid.NewString(),
				[]string{maya, w.people["ada"].String(), w.people["ben"].String()})
		}, false},
	}
	for _, s := range steps {
		ada, ben := w.emailsTo(t, "ada"), w.emailsTo(t, "ben")
		s.do()
		helpers.SettleBackground(10 * time.Second)
		if s.benHear && !within(20*time.Second, func() bool { return w.emailsTo(t, "ben") > ben }) {
			t.Errorf("%s: Ben (Mentions), whom it names, wasn't emailed", s.what)
		}
		if !s.benHear && w.emailsTo(t, "ben") != ben {
			t.Errorf("%s: Ben (Mentions) was emailed what doesn't name him", s.what)
		}
		if got := w.emailsTo(t, "ada") - ada; got != 0 {
			t.Errorf("%s: Ada set the project to Nothing, yet was emailed %d times", s.what, got)
		}
	}
}

// (d) Ada and Ben have each paused notifications. Maya has a DM with each;
// Ada set hers to "Nothing", Ben his to "Mentions". Notify anyway reaches Ben
// and is refused for Ada, whose device gets no urgent push.
func TestMutedDMRefusesNotifyAnyway(t *testing.T) {
	w, _ := newEditWorld(t)
	maya := w.as(t, "maya").Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	for name, level := range map[string]string{"ada": "block", "ben": "mention"} {
		w.send(t, "maya", chatController.CreateChat, http.MethodPost, chatAdapter.ChatInfo{ToUuid: w.people[name].String(), TextHtml: "<p>Lunch?</p>"})
		helpers.SettleBackground(10 * time.Second) // the DM's settings are written with it
		grp := helpers.GetGroupingId(w.people["maya"].String(), w.people[name].String())
		w.exec(t, `UPDATE users_chat_notification SET notification_type = $3 WHERE user_id = $1 AND grp_id = $2`, w.people[name], grp, level)
		w.exec(t, `UPDATE users_notification_preferences SET notifications_paused_until = NOW() + INTERVAL '1 hour' WHERE user_id = $1`, w.people[name])
	}
	helpers.SettleBackground(10 * time.Second)
	urgent := func(name string) int {
		w.mu.Lock()
		defer w.mu.Unlock()
		n := 0
		for _, p := range w.pushes {
			if !strings.HasPrefix(p.data[firebaseInit.FIREBASE_PUSH_DATA_TITLE], "Urgent from") {
				continue
			}
			for _, tok := range p.tokens {
				if tok == "tok-"+name {
					n++
				}
			}
		}
		return n
	}

	if err := notification.NotifyAnyway(context.Background(), &maya, w.people["ben"], time.Now()); err != nil {
		t.Fatalf("Notify anyway to Ben (Mentions): %v", err)
	}
	if !within(20*time.Second, func() bool { return urgent("ben") > 0 }) {
		t.Fatal("Ben (Mentions) wasn't pushed the urgent ping")
	}
	err := notification.NotifyAnyway(context.Background(), &maya, w.people["ada"], time.Now())
	var refused *notification.UrgentError
	if !errors.As(err, &refused) {
		t.Errorf("Ada set the DM to Nothing, yet Notify anyway answered %v, not a refusal", err)
	}
	helpers.SettleBackground(10 * time.Second)
	if got := urgent("ada"); got != 0 {
		t.Errorf("Ada set the DM to Nothing, yet got %d urgent pushes", got)
	}
}
