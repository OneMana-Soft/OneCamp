//go:build integration
// +build integration

package integration_test

// Told means listed, for mentions (NOTIF-H3): whoever a mention names is told
// once, and finds it in their Inbox and on their badge.
//
// A task's description wrote its mentions where no feed reads them (the graph
// feed keeps mentions in messages and comments), so the person it named was
// pushed and found nothing after a reload. An edit that adds someone to a
// task, a channel's message or a conversation's message rewrote the mention in
// place: nobody was pushed or emailed, and the person added was listed at the
// message's first time, under their last look, so neither the Inbox's dots nor
// the badge showed it.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestATaskMentionIsListedAndAnAddedOneToldOnce|TestAMentionAnEditAddsIsToldOnceAtTheEdit' -v

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

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// inboxAbout is the items of name's Inbox (the feed with its notices, as the
// app reads it) about the thing id names, and their badge.
func inboxAbout(t *testing.T, user uuid.UUID, uid, id string) ([]dgraphModels.UnifiedActivityItem, uint64) {
	t.Helper()
	ctx := context.Background()
	page, err := activityBusiness.GetUnifiedActivity(ctx, activityBusiness.FeedQuery{User: user, DgraphID: uid,
		Before: time.Now().Add(time.Minute), Limit: 50, Notices: true})
	if err != nil {
		t.Fatalf("the feed: %v", err)
	}
	badge, err := activityBusiness.UnreadActivityCount(ctx, user, uid)
	if err != nil {
		t.Fatalf("the badge: %v", err)
	}
	var about []dgraphModels.UnifiedActivityItem
	for _, it := range page.Activities {
		if itemIsAbout(it, id) {
			about = append(about, it)
		}
	}
	return about, badge
}

// itemIsAbout is whether a feed item is about the thing id names: a mention
// in it, or a notice of it.
func itemIsAbout(it dgraphModels.UnifiedActivityItem, id string) bool {
	if n := it.Notice; n != nil {
		if n.ResourceUUID == id {
			return true
		}
		for _, v := range n.Data {
			if v == id {
				return true
			}
		}
	}
	if m := it.Mention; m != nil {
		switch id {
		case m.PostUuid, m.ChatUuid, m.TaskUuid, m.CommentUuid:
			return true
		}
		if (m.Post != nil && m.Post.Uuid == id) || (m.Chat != nil && m.Chat.Uuid == id) || (m.Task != nil && m.Task.Uuid == id) {
			return true
		}
	}
	return false
}

// pushesTo is how many pushes reached the device token of the kind of thing
// with this id (its type_id or thread_id).
func (w *taskWorld) pushesTo(kind, id, token string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, p := range w.pushes {
		if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == kind &&
			(p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] == id || p.data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] == id) {
			for _, tok := range p.tokens {
				if tok == token {
					n++
				}
			}
		}
	}
	return n
}

// Maya makes a task that names Mo, who hears of mentions only: Mo is pushed
// and, after a reload, finds it in the Inbox and on the badge. Her edit adds
// Tom: Tom is pushed once and finds it once. A save that changes the words, and
// one that takes Tom out and one that puts him back, tell nobody again.
func TestATaskMentionIsListedAndAnAddedOneToldOnce(t *testing.T) {
	w := newTaskWorld(t)
	desc := func(words string, names ...string) string {
		var b strings.Builder
		b.WriteString("<p>" + words)
		for _, n := range names {
			b.WriteString(" " + w.mention(n))
		}
		return b.String() + "</p>"
	}
	task := w.call(t, taskController.CreateTask, map[string]any{"task_project_uuid": w.launch, "task_name": "Venue contract",
		"task_description": desc("Needs eyes", "mo")})["task_uuid"]
	if !within(20*time.Second, func() bool { return w.pushesTo("task", task, "tok-mo") > 0 }) {
		t.Fatal("Mo, named in the new task, wasn't pushed")
	}
	if about, badge := inboxAbout(t, w.people["mo"], w.uids["mo"], task); len(about) != 1 || badge != 1 {
		t.Errorf("Mo, named in the new task: %d items about it, badge %d; want it listed once and a badge of 1", len(about), badge)
	}

	edit := func(html string) {
		t.Helper()
		w.call(t, taskController.UpdateTaskDesc, map[string]any{"task_uuid": task, "task_project_uuid": w.launch, "task_description": html})
	}
	edit(desc("Needs eyes", "mo", "tom"))
	if !within(20*time.Second, func() bool { return w.pushesTo("task", task, "tok-tom") > 0 }) {
		t.Error("Tom, whom the edit names, wasn't pushed")
	}
	edit(desc("Needs two pairs of eyes", "mo", "tom"))
	edit(desc("Needs eyes", "mo"))
	edit(desc("Needs eyes", "mo", "tom"))
	time.Sleep(2 * time.Second)

	if n := w.pushesTo("task", task, "tok-tom"); n != 1 {
		t.Errorf("Tom was pushed %d times; want once: a re-save, and a remove and re-add, tell nobody again", n)
	}
	if n := w.pushesTo("task", task, "tok-mo"); n != 1 {
		t.Errorf("Mo was pushed %d times; want once, when the task was made", n)
	}
	for _, name := range []string{"mo", "tom"} {
		if about, badge := inboxAbout(t, w.people[name], w.uids[name], task); len(about) != 1 || badge != 1 {
			t.Errorf("%s: %d items about the task, badge %d; want it listed once and a badge of 1", name, len(about), badge)
		}
	}
	if about, badge := inboxAbout(t, w.people["maya"], w.uids["maya"], task); len(about) != 0 || badge != 0 {
		t.Errorf("Maya, who named them: %d items about the task, badge %d; want none", len(about), badge)
	}
}

// editWorld is #launch and a group, each with Maya, Ada and Ben in it. Ada
// and Ben hear of mentions only in #launch, and Ben of mentions only in the
// group.
type editWorld struct {
	env    *integration.Env
	dg     *integration.DgraphEnv
	people map[string]uuid.UUID
	uids   map[string]string

	mu     sync.Mutex
	pushes []sentPush
}

func newEditWorld(t *testing.T) (*editWorld, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	t.Setenv("RESEND_API_KEY", "re_integration_not_a_key") // notification email on; nothing is sent from a test
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	w := &editWorld{env: integration.SetupEnv(t), people: map[string]uuid.UUID{}, uids: map[string]string{}}
	if err := postgresInit.ConnectPostgres(ctx, w.env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	w.dg = dg
	t.Cleanup(func() { helpers.SettleBackground(10 * time.Second) })
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
		w.pushes = append(w.pushes, sentPush{data, tokens})
		w.mu.Unlock()
	}))

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	channel := uuid.New()
	exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, channel)
	levels := map[string]string{"maya": "all", "ada": "mention", "ben": "mention"}
	var nodes []map[string]any
	for _, name := range []string{"maya", "ada", "ben"} {
		id := uuid.New()
		w.people[name] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
		exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, 'unsub-' || $2)`, id, name)
		exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-' || $2)`, id, name)
		exec(`INSERT INTO users_channel_notification (user_id, channel_id, notification_type) VALUES ($1, $2, $3)`, id, channel, levels[name])
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name, "user_deleted_at": liveAt})
	}
	members := []map[string]any{{"uid": "_:maya"}, {"uid": "_:ada"}, {"uid": "_:ben"}}
	got := dg.Mutate(t, append(nodes, map[string]any{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(), "ch_name": "launch",
		"ch_private": false, "ch_deleted_at": liveAt, "ch_members": members}))
	for name := range w.people {
		w.uids[name] = got[name]
	}
	return w, channel
}

func (w *editWorld) as(t *testing.T, name string) context.Context {
	t.Helper()
	ctx := context.Background()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, w.people[name])
	if err != nil || pg == nil {
		t.Fatalf("load %s: %v", name, err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, w.people[name].String())
	if err != nil || gr == nil {
		t.Fatalf("load %s's node: %v", name, err)
	}
	return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
}

// send runs handler as name and answers what it made (its uuid), failing on
// anything but a 200.
func (w *editWorld) send(t *testing.T, name string, handler http.HandlerFunc, method string, body any) string {
	t.Helper()
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(method, "/", strings.NewReader(string(raw))).WithContext(w.as(t, name)))
	var out struct {
		Data struct {
			Uuid string `json:"uuid"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	return out.Data.Uuid
}

func (w *editWorld) chip(name string) string {
	return fmt.Sprintf(`<span data-type="mention" data-id="%s@%s">@%s</span>`, w.people[name], w.uids[name], name)
}

// pushesOf is how many pushes about the message reached name's device.
func (w *editWorld) pushesOf(name, message string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, p := range w.pushes {
		if p.data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] != message {
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

// emailsOf is how many emails about the message are queued for name.
func (w *editWorld) emailsOf(t *testing.T, name, message string) int {
	t.Helper()
	var n int
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM notification_email_queue WHERE user_id = $1 AND strpos(dedup_key, $2) > 0`,
		w.people[name], message).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Maya names Ada in a message in #launch, and in one in their group. Ada and
// Ben look at their Inbox. Maya's edit of each adds Ben: Ben is pushed and
// emailed once, and finds ONE item about it, at the edit, with a badge of 1.
// Ada, named already, is told nothing more.
func TestAMentionAnEditAddsIsToldOnceAtTheEdit(t *testing.T) {
	w, channel := newEditWorld(t)

	post := w.send(t, "maya", postController.CreatePost, http.MethodPost,
		postAdapter.InputCreateOrUpdatePostInfo{ChannelUuid: channel.String(), HTMLText: "<p>Venue is booked " + w.chip("ada") + "</p>"})
	chat := w.send(t, "maya", chatController.CreateGroupChat, http.MethodPost, chatAdapter.ChatInfo{
		Participants: []string{w.uids["maya"], w.uids["ada"], w.uids["ben"]}, TextHtml: "<p>Our plan " + w.chip("ada") + "</p>"})
	grp, _ := helpers.GenerateGroupID([]string{w.people["maya"].String(), w.people["ada"].String(), w.people["ben"].String()})
	if res, err := w.env.PG.Exec(`UPDATE users_chat_notification SET notification_type = 'mention' WHERE user_id = $1 AND grp_id = $2`, w.people["ben"], grp); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("Ben's setting for the group: %d rows", n)
	}
	if !within(20*time.Second, func() bool { return w.pushesOf("ada", post) > 0 && w.pushesOf("ada", chat) > 0 }) {
		t.Fatal("Ada, named in both messages, wasn't pushed")
	}
	helpers.SettleBackground(10 * time.Second)

	type before struct{ pushes, emails int }
	was := map[string]map[string]before{}
	for _, name := range []string{"ada", "ben"} {
		if err := activityBusiness.MarkActivitySeen(context.Background(), w.people[name]); err != nil {
			t.Fatal(err)
		}
		was[name] = map[string]before{post: {w.pushesOf(name, post), w.emailsOf(t, name, post)}, chat: {w.pushesOf(name, chat), w.emailsOf(t, name, chat)}}
	}
	time.Sleep(50 * time.Millisecond)
	edited := time.Now()

	w.send(t, "maya", postController.UpdatePost, http.MethodPut,
		postAdapter.InputCreateOrUpdatePostInfo{Uuid: post, HTMLText: "<p>Venue is booked " + w.chip("ada") + " " + w.chip("ben") + "</p>"})
	if !within(20*time.Second, func() bool { return w.pushesOf("ben", post) > was["ben"][post].pushes }) {
		t.Error("Ben, whom the channel message's edit names, wasn't pushed")
	}
	w.send(t, "maya", chatController.UpdateChatInGroup, http.MethodPost,
		chatAdapter.ChatInfo{Uuid: chat, GrpUuid: grp, TextHtml: "<p>Our plan " + w.chip("ada") + " " + w.chip("ben") + "</p>"})
	if !within(20*time.Second, func() bool { return w.pushesOf("ben", chat) > was["ben"][chat].pushes }) {
		t.Error("Ben, whom the group message's edit names, wasn't pushed")
	}
	helpers.SettleBackground(10 * time.Second)

	for _, m := range []struct{ what, id string }{{"the channel message", post}, {"the group message", chat}} {
		if got := w.pushesOf("ben", m.id) - was["ben"][m.id].pushes; got != 1 {
			t.Errorf("Ben was pushed %d times of %s's edit; want once", got, m.what)
		}
		about, _ := inboxAbout(t, w.people["ben"], w.uids["ben"], m.id)
		if len(about) != 1 {
			t.Errorf("Ben has %d items about %s; want one", len(about), m.what)
		} else if at, err := time.Parse(time.RFC3339Nano, about[0].Time); err != nil || at.Before(edited) {
			t.Errorf("Ben's item about %s is at %s; want the edit's time (after %s)", m.what, about[0].Time, edited.UTC().Format(time.RFC3339Nano))
		}
		if got := w.emailsOf(t, "ben", m.id) - was["ben"][m.id].emails; got != 1 {
			t.Errorf("Ben was emailed %d times of %s's edit; want once", got, m.what)
		}
		if got := w.pushesOf("ada", m.id) - was["ada"][m.id].pushes; got != 0 {
			t.Errorf("Ada, named in %s already, was pushed %d times by the edit; want none", m.what, got)
		}
		if got := w.emailsOf(t, "ada", m.id) - was["ada"][m.id].emails; got != 0 {
			t.Errorf("Ada, named in %s already, was emailed %d times by the edit; want none", m.what, got)
		}
	}
	if _, badge := inboxAbout(t, w.people["ben"], w.uids["ben"], post); badge != 2 {
		t.Errorf("Ben's badge is %d; want 2, one for each message's edit", badge)
	}
	if _, badge := inboxAbout(t, w.people["ada"], w.uids["ada"], post); badge != 0 {
		t.Errorf("Ada's badge is %d; want 0: the edits named nobody new to her", badge)
	}
}
