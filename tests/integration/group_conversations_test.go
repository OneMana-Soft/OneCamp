//go:build integration
// +build integration

package integration_test

// Group conversations done Slack's way (D-S1; chat A9), against real Postgres
// 12, Dgraph and Redis: the programme's acceptance (PROGRAM.md A9, "Tests").
//
// Maya, Jonas and Sam talk. Sam adds Priya with "No, start fresh": Priya
// can't read, open the files of, or type in the old conversation, which keeps
// its people, its messages and its id; the new one, for the four of them,
// says how it began, and notifies all four. With "The last 7 days" Priya gets
// exactly those messages, their replies and files, and none of what they
// can't show her (a forward, a poll's votes, a quote of something unshared);
// copies notify nobody, and a share run twice copies once. A group can be
// named, hidden until a newer message, and made into a private channel with
// its history; an external identity is never added.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAGroupIsAFixedSetOfPeople -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	dmAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestAGroupIsAFixedSetOfPeople(t *testing.T) {
	ctx := context.Background()
	w := newIndexWorld(t, "maya", "jonas", "sam", "priya", "ext")
	for _, n := range []string{"maya", "jonas", "sam", "priya"} {
		if _, err := w.env.PG.Exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-' || $2)`, w.people[n], n); err != nil {
			t.Fatal(err)
		}
	}
	// An identity imported from elsewhere, who never signs in.
	if _, err := w.env.PG.Exec(`UPDATE users SET is_external = true WHERE id = $1`, w.people["ext"]); err != nil {
		t.Fatal(err)
	}
	w.dg.Mutate(t, map[string]any{"uid": w.uids["ext"], "is_external": true})

	var mu sync.Mutex
	var pushes []sentPush
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		mu.Lock()
		pushes = append(pushes, sentPush{data, tokens})
		mu.Unlock()
	})()
	pushedFor := func(messageID string) []string {
		mu.Lock()
		defer mu.Unlock()
		var to []string
		for _, p := range pushes {
			if p.data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] == messageID {
				for _, tok := range p.tokens {
					to = append(to, strings.TrimPrefix(tok, "tok-"))
				}
			}
		}
		return to
	}
	people := map[string]*userModels.UserInfo{}
	for _, n := range []string{"maya", "jonas", "sam", "priya"} {
		people[n] = w.info(t, n)
	}
	call := func(who string, handler http.HandlerFunc, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, *people[who])))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", who, rec.Code, rec.Body.String())
		}
		var answer struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		return answer.Data
	}
	group := func(names ...string) string {
		var ids []string
		for _, n := range names {
			ids = append(ids, w.people[n].String())
		}
		id, err := helpers.GenerateGroupID(ids)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	// Maya, Jonas and Sam talk: a kickoff with a reply, a file, a poll, a
	// message from ten days ago, and a forward.
	old := group("maya", "jonas", "sam")
	kickoff := call("maya", chatController.CreateGroupChat, dmAdapter.ChatInfo{
		Participants: []string{w.uids["maya"], w.uids["jonas"], w.uids["sam"]}, TextHtml: "<p>Kickoff on Friday</p>"})["uuid"].(string)
	call("sam", chatController.CreateChatComment, dmAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: kickoff, HTMLText: "<p>Works for me</p>"})
	fileRow := uuid.New()
	if _, err := w.env.PG.Exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, 'jonas/plan.pdf', 'grpChat', $2, $3)`,
		fileRow, old, w.people["jonas"]); err != nil {
		t.Fatal(err)
	}
	withFile := call("jonas", chatController.CreateGroupChat, dmAdapter.ChatInfo{GrpUuid: old, TextHtml: "<p>The plan</p>",
		MediaObjects: []*dgraphStruct.DgraphAttachment{{Uuid: fileRow.String(), ObjectKey: "jonas/plan.pdf", FileName: "plan.pdf"}}})["uuid"].(string)
	poll := call("sam", chatController.CreateGroupChat, dmAdapter.ChatInfo{GrpUuid: old,
		TextHtml: `<p><strong>Poll:</strong> Friday or Monday?</p><div data-type="poll" data-id="` + uuid.NewString() + `"></div>`})["uuid"].(string)
	tenDaysAgo := call("maya", chatController.CreateGroupChat, dmAdapter.ChatInfo{GrpUuid: old, TextHtml: "<p>Old news</p>"})["uuid"].(string)
	long := time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	w.dg.Mutate(t, map[string]any{"uid": chatUID(t, w, tenDaysAgo), "chat_created_at": long})
	forwardOf := chatUID(t, w, kickoff)
	oldUID := dmUID(t, w, old)
	w.dg.Mutate(t, map[string]any{"uid": oldUID, "dm_chats": []map[string]any{{
		"uid": "_:fw", "dgraph.type": "Chat", "chat_uuid": uuid.NewString(), "chat_body_text": "",
		"chat_created_at": time.Now().UTC().Format(time.RFC3339Nano), "chat_deleted_at": "0001-01-01T00:00:00Z",
		"chat_from": map[string]any{"uid": w.uids["jonas"]}, "chat_dm": map[string]any{"uid": oldUID},
		"chat_fwd_msg_chat": map[string]any{"uid": forwardOf},
	}}})
	// Jonas hears of the group only when mentioned.
	if _, err := w.env.PG.Exec(`UPDATE users_chat_notification SET notification_type = 'mention' WHERE grp_id = $1 AND user_id = $2`, old, w.people["jonas"]); err != nil {
		t.Fatal(err)
	}

	four := group("maya", "jonas", "sam", "priya")
	oldRef := conversationBusiness.Ref{Kind: conversationBusiness.KindGroup, ID: old}
	newRef := conversationBusiness.Ref{Kind: conversationBusiness.KindGroup, ID: four}

	t.Run("an outsider can't type in a group by working out its id", func(t *testing.T) {
		if conversationBusiness.MayType(ctx, people["priya"], oldRef) {
			t.Error("Priya, outside the group, may say she's typing in it")
		}
		if !conversationBusiness.MayType(ctx, people["sam"], oldRef) {
			t.Error("Sam can't type in his own group")
		}
	})

	t.Run("an external identity is never added", func(t *testing.T) {
		_, err := conversationBusiness.AddPeople(ctx, people["maya"], oldRef, []string{w.people["ext"].String()}, conversationBusiness.ShareNone)
		var rj *sendBusiness.Rejection
		if !errors.As(err, &rj) || rj.Status != http.StatusBadRequest {
			t.Fatalf("adding an external identity: %v", err)
		}
	})

	t.Run("adding Priya with no history opens the four's conversation and leaves the old one as it was", func(t *testing.T) {
		opened, err := conversationBusiness.AddPeople(ctx, people["sam"], oldRef, []string{w.people["priya"].String()}, conversationBusiness.ShareNone)
		if err != nil || opened.ID != four || opened.Existed || opened.Sharing != 0 {
			t.Fatalf("opened %+v (%v)", opened, err)
		}
		sameList(t, "the old group's people", w.members(t, old), "maya:member", "jonas:member", "sam:member")
		sameList(t, "the new group's people", w.members(t, four), "maya:member", "jonas:member", "sam:member", "priya:member")
		if _, err := conversationBusiness.Resolve(ctx, people["priya"], oldRef); !errors.Is(err, conversationBusiness.ErrNotFound) {
			t.Errorf("Priya reads the old group: %v", err)
		}
		if d, err := conversationBusiness.Resolve(ctx, people["priya"], newRef); err != nil || d.Level < conversationBusiness.LevelPost {
			t.Errorf("Priya in the new group: %+v %v", d, err)
		}
		if _, err := fileAccessBusiness.ReadFile(ctx, w.people["priya"].String(), fileAccessBusiness.ConversationOf(old), fileRow.String()); !fileAccessBusiness.IsRefusal(err) {
			t.Errorf("Priya opens the old group's file: %v", err)
		}
		if n := copiesIn(t, w, four); n != 0 {
			t.Errorf("%d messages were copied with no history chosen", n)
		}
		if lines := systemLines(t, w, old); len(lines) != 1 || !strings.Contains(lines[0], `"group.continued"`) {
			t.Errorf("the old group's lines: %v", lines)
		}
		if lines := systemLines(t, w, four); len(lines) != 1 || !strings.Contains(lines[0], `"group.started"`) {
			t.Errorf("the new group's lines: %v", lines)
		}
		var level string
		if err := w.env.PG.QueryRow(`SELECT notification_type FROM users_chat_notification WHERE grp_id = $1 AND user_id = $2`, four, w.people["jonas"]).Scan(&level); err != nil || level != "mention" {
			t.Errorf("Jonas's setting in the new group: %q (%v), want the one he chose for the old", level, err)
		}
		// The old group's messages are where they were, under its own id.
		if got := chatsIn(t, w, old); got < 5 {
			t.Errorf("the old group holds %d messages", got)
		}
	})

	t.Run("sharing the last 7 days copies exactly those messages, their replies and files, once", func(t *testing.T) {
		choices, err := conversationBusiness.ShareChoices(ctx, people["maya"], oldRef)
		if err != nil {
			t.Fatal(err)
		}
		week := choiceOf(choices, conversationBusiness.ShareWeek)
		all := choiceOf(choices, conversationBusiness.ShareAll)
		// Kickoff, the plan and the poll, and the kickoff's reply; never the
		// forward, nor a system line; the old news only in everything.
		if week.Messages != 3 || week.Replies != 1 || all.Messages != 4 {
			t.Fatalf("choices: week %+v, everything %+v", week, all)
		}
		opened, err := conversationBusiness.AddPeople(ctx, people["maya"], oldRef, []string{w.people["priya"].String()}, conversationBusiness.ShareWeek)
		if err != nil || opened.ID != four || !opened.Existed || opened.Sharing != 4 {
			t.Fatalf("opened %+v (%v)", opened, err)
		}
		before := len(pushes)
		drainJobs(t, ctx)
		if n := copiesIn(t, w, four); n != 4 {
			t.Fatalf("%d copies, want 4", n)
		}
		copies := copiesByOriginal(t, w, four)
		for _, original := range []string{kickoff, withFile, poll} {
			if copies[original] == "" {
				t.Errorf("%s wasn't copied", original)
			}
		}
		if copies[tenDaysAgo] != "" {
			t.Error("a message from ten days ago was shared with the last 7 days")
		}
		if body := chatBody(t, w, copies[poll]); strings.Contains(body, "data-type") || !strings.Contains(body, "Friday or Monday?") {
			t.Errorf("the poll's copy: %q", body)
		}
		if replies := repliesOf(t, w, copies[kickoff]); len(replies) != 1 {
			t.Errorf("the kickoff's copy has %d replies", len(replies))
		}
		copiedRow := fileRowOf(t, w, four, "jonas/plan.pdf")
		if copiedRow == "" {
			t.Fatal("the plan's copy has no file row in the new group")
		}
		if _, err := fileAccessBusiness.ReadFile(ctx, w.people["priya"].String(), fileAccessBusiness.ConversationOf(four), copiedRow); err != nil {
			t.Errorf("Priya can't open the shared file: %v", err)
		}
		if _, err := fileAccessBusiness.ReadFile(ctx, w.people["priya"].String(), fileAccessBusiness.ConversationOf(old), fileRow.String()); !fileAccessBusiness.IsRefusal(err) {
			t.Errorf("Priya opens the original file in the old group: %v", err)
		}
		mu.Lock()
		after := pushes[before:]
		mu.Unlock()
		for _, p := range after {
			for _, c := range copies {
				if p.data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] == c {
					t.Errorf("a copy was pushed: %+v", p)
				}
			}
		}
		if lines := systemLines(t, w, four); len(lines) != 2 || !strings.Contains(lines[1], `"history.shared"`) {
			t.Errorf("the new group's lines after the share: %v", lines)
		}
		// Asked for again, the share copies nothing more.
		if _, err := conversationBusiness.AddPeople(ctx, people["maya"], oldRef, []string{w.people["priya"].String()}, conversationBusiness.ShareWeek); err != nil {
			t.Fatal(err)
		}
		drainJobs(t, ctx)
		if n := copiesIn(t, w, four); n != 4 {
			t.Errorf("a second share left %d copies, want 4", n)
		}
		if lines := systemLines(t, w, four); len(lines) != 2 {
			t.Errorf("a share that made nothing said so: %v", lines)
		}
	})

	t.Run("a message in the new group reaches the other three", func(t *testing.T) {
		// Jonas hears of the group only when mentioned, as he chose for the
		// one before: he is mentioned.
		jonas := `<span data-type="mention" data-id="` + w.people["jonas"].String() + `@` + w.uids["jonas"] + `">@jonas</span>`
		sent := call("maya", chatController.CreateGroupChat, dmAdapter.ChatInfo{GrpUuid: four, TextHtml: "<p>Welcome, Priya, and " + jonas + "</p>"})["uuid"].(string)
		waitFor(t, "the welcome reaching the other three", func() bool { return len(pushedFor(sent)) == 3 })
		sameList(t, "pushed to", pushedFor(sent), "jonas", "sam", "priya")
	})

	t.Run("a group can be named, and hidden until a newer message", func(t *testing.T) {
		if err := conversationBusiness.Rename(ctx, people["sam"], newRef, "  Launch   crew "); err != nil {
			t.Fatal(err)
		}
		if got := groupName(t, w, four); got != "Launch crew" {
			t.Errorf("the group's name: %q", got)
		}
		if err := conversationBusiness.SetHidden(ctx, people["priya"], newRef, true); err != nil {
			t.Fatal(err)
		}
		if listed(t, ctx, people["priya"], four) {
			t.Error("a hidden group is still in Priya's list")
		}
		call("jonas", chatController.CreateGroupChat, dmAdapter.ChatInfo{GrpUuid: four, TextHtml: "<p>Still there?</p>"})
		if !listed(t, ctx, people["priya"], four) {
			t.Error("a newer message didn't bring the group back")
		}
	})

	t.Run("a group made a private channel takes its people and, asked, its history", func(t *testing.T) {
		opened, err := conversationBusiness.MakePrivateChannel(ctx, people["maya"], newRef, nil, "launch-crew", true)
		if err != nil || opened.Kind != conversationBusiness.KindChannel || opened.Sharing == 0 {
			t.Fatalf("opened %+v (%v)", opened, err)
		}
		drainJobs(t, ctx)
		sameList(t, "the channel's people", w.members(t, opened.ID), "maya:manager", "jonas:member", "sam:member", "priya:member")
		var copied int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM posts WHERE post_channel = $1`, opened.ID).Scan(&copied); err != nil || copied == 0 {
			t.Errorf("the channel holds %d copied messages (%v)", copied, err)
		}
		var continued string
		if err := w.env.PG.QueryRow(`SELECT c2.ref FROM conversations c JOIN conversations c2 ON c2.id = c.continued_in WHERE c.ref = $1`, four).Scan(&continued); err != nil || continued != opened.ID {
			t.Errorf("the group continues in %q (%v)", continued, err)
		}
	})
}

// drainJobs runs the scheduler's due jobs until none is left: a share's runs.
func drainJobs(t *testing.T, ctx context.Context) {
	t.Helper()
	for i := 0; i < 20; i++ {
		if schedulerBusiness.DrainDueJobs(ctx, "integration-test") == 0 {
			return
		}
	}
	t.Fatal("the scheduler kept finding work")
}

func choiceOf(choices []conversationBusiness.ShareChoice, s conversationBusiness.Share) conversationBusiness.ShareChoice {
	for _, c := range choices {
		if c.Share == s {
			return c
		}
	}
	return conversationBusiness.ShareChoice{}
}

func listed(t *testing.T, ctx context.Context, who *userModels.UserInfo, grp string) bool {
	t.Helper()
	id, _ := uuid.Parse(who.UserDgraphInfo.Uuid)
	user, err := chatBusiness.GetUserChatListWithLatestChat(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, dm := range user.DMs {
		if dm.GroupingId == grp {
			return true
		}
	}
	return false
}

// The graph's side, read as the tests need it.

func chatUID(t *testing.T, w *indexWorld, chatUUID string) string {
	t.Helper()
	var got struct {
		Q []struct {
			UID string `json:"uid"`
		} `json:"q"`
	}
	_ = json.Unmarshal(w.dg.Query(t, `{ q(func: eq(chat_uuid, "`+chatUUID+`")) { uid } }`), &got)
	if len(got.Q) != 1 {
		t.Fatalf("no message %s", chatUUID)
	}
	return got.Q[0].UID
}

func dmUID(t *testing.T, w *indexWorld, grp string) string {
	t.Helper()
	var got struct {
		Q []struct {
			UID string `json:"uid"`
		} `json:"q"`
	}
	_ = json.Unmarshal(w.dg.Query(t, `{ q(func: eq(dm_grouping_id, "`+grp+`")) { uid } }`), &got)
	if len(got.Q) != 1 {
		t.Fatalf("no conversation %s", grp)
	}
	return got.Q[0].UID
}

type groupChats struct {
	Q []struct {
		Name  string `json:"dm_name"`
		Chats []struct {
			UUID    string `json:"chat_uuid"`
			Body    string `json:"chat_body_text"`
			Kind    string `json:"chat_kind"`
			System  string `json:"chat_system"`
			CopyOf  string `json:"chat_copy_of"`
			Replies []struct {
				UUID string `json:"comment_uuid"`
			} `json:"chat_comments"`
		} `json:"dm_chats"`
	} `json:"q"`
}

func readGroup(t *testing.T, w *indexWorld, grp string) groupChats {
	t.Helper()
	var got groupChats
	_ = json.Unmarshal(w.dg.Query(t, `{ q(func: eq(dm_grouping_id, "`+grp+`")) { dm_name
		dm_chats (orderasc: chat_created_at) { chat_uuid chat_body_text chat_kind chat_system chat_copy_of chat_comments { comment_uuid } } } }`), &got)
	return got
}

func systemLines(t *testing.T, w *indexWorld, grp string) []string {
	t.Helper()
	var out []string
	for _, q := range readGroup(t, w, grp).Q {
		for _, c := range q.Chats {
			if c.Kind == "system" {
				out = append(out, c.System)
			}
		}
	}
	return out
}

func chatsIn(t *testing.T, w *indexWorld, grp string) int {
	t.Helper()
	n := 0
	for _, q := range readGroup(t, w, grp).Q {
		n += len(q.Chats)
	}
	return n
}

func copiesByOriginal(t *testing.T, w *indexWorld, grp string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, q := range readGroup(t, w, grp).Q {
		for _, c := range q.Chats {
			if c.CopyOf != "" {
				out[c.CopyOf] = c.UUID
			}
		}
	}
	return out
}

func chatBody(t *testing.T, w *indexWorld, chatUUID string) string {
	t.Helper()
	var got struct {
		Q []struct {
			Body string `json:"chat_body_text"`
		} `json:"q"`
	}
	_ = json.Unmarshal(w.dg.Query(t, `{ q(func: eq(chat_uuid, "`+chatUUID+`")) { chat_body_text } }`), &got)
	if len(got.Q) != 1 {
		return ""
	}
	return got.Q[0].Body
}

func repliesOf(t *testing.T, w *indexWorld, chatUUID string) []string {
	t.Helper()
	var got struct {
		Q []struct {
			Replies []struct {
				UUID string `json:"comment_uuid"`
			} `json:"chat_comments"`
		} `json:"q"`
	}
	_ = json.Unmarshal(w.dg.Query(t, `{ q(func: eq(chat_uuid, "`+chatUUID+`")) { chat_comments { comment_uuid } } }`), &got)
	var out []string
	for _, q := range got.Q {
		for _, r := range q.Replies {
			out = append(out, r.UUID)
		}
	}
	return out
}

func groupName(t *testing.T, w *indexWorld, grp string) string {
	t.Helper()
	for _, q := range readGroup(t, w, grp).Q {
		return q.Name
	}
	return ""
}

// The index's side.

func copiesIn(t *testing.T, w *indexWorld, ref string) int {
	t.Helper()
	var n int
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE c.ref = $1 AND m.copied_from IS NOT NULL`, ref).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func fileRowOf(t *testing.T, w *indexWorld, grp, objKey string) string {
	t.Helper()
	var id string
	_ = w.env.PG.QueryRow(`SELECT id::text FROM attachments WHERE src_key = 'grpChat' AND src_value = $1 AND obj_key = $2`, grp, objKey).Scan(&id)
	return id
}
