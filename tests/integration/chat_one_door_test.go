//go:build integration
// +build integration

package integration_test

// One door for chat (A4 H2): every chat notice goes through Notices.Plan and
// Deliver, which writes one notification_deliveries row per person and
// delivery. One test per cause, each through the routes people use, reading
// the rows the router wrote; then a double fire, and a soft delete.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	notifyBusiness "github.com/akashc777/OneCamp/business/Notify"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	conversationController "github.com/akashc777/OneCamp/controllers/Conversation"
	notificationController "github.com/akashc777/OneCamp/controllers/Notification"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/go-chi/chi/v5"
)

// rowsFor is the delivery rows of the message, as "name delivery cause
// state", sorted, once at least want of them are written (or 10 s), and a
// moment more for any that shouldn't be.
func (w *reachWorld) rowsFor(t *testing.T, messageID string, want int) []string {
	t.Helper()
	read := func() []string {
		rows, err := w.env.PG.Query(`SELECT user_id::text, delivery, cause, state FROM notification_deliveries WHERE message_id = $1`, messageID)
		if err != nil {
			t.Fatalf("read the rows: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var user, delivery, cause, state string
			if err := rows.Scan(&user, &delivery, &cause, &state); err != nil {
				t.Fatal(err)
			}
			out = append(out, strings.Join([]string{w.names[user], delivery, cause, state}, " "))
		}
		sort.Strings(out)
		return out
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && len(read()) < want; {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	return read()
}

// rowsOf is name's rows among rows.
func rowsOf(rows []string, name string) []string {
	var out []string
	for _, r := range rows {
		if strings.HasPrefix(r, name+" ") {
			out = append(out, r)
		}
	}
	return out
}

// toldRows is the rows a person told by cause, now, gets for deliveries.
func toldRows(name string, cause notifyBusiness.Cause, deliveries ...string) []string {
	var out []string
	for _, d := range deliveries {
		out = append(out, fmt.Sprintf("%s %s %s sent", name, d, cause))
	}
	sort.Strings(out)
	return out
}

var (
	loudRows  = []string{"badge", "desktop", "inbox", "push", "sound"}
	namedRows = []string{"badge", "desktop", "email", "inbox", "push", "sound"}
)

// postAs posts in #launch as ctx's person, answering the post's id.
func (w *reachWorld) postAs(t *testing.T, ctx context.Context, in postAdapter.InputCreateOrUpdatePostInfo) string {
	t.Helper()
	in.ChannelUuid = w.channel.String()
	code, data, msg := w.run(ctx, postController.CreatePost, http.MethodPost, "", in)
	var created struct {
		Uuid string `json:"uuid"`
	}
	_ = json.Unmarshal(data, &created)
	if code != http.StatusOK && code != http.StatusCreated || created.Uuid == "" {
		t.Fatalf("post: %d %s", code, msg)
	}
	return created.Uuid
}

// replyAs replies in the post's thread as ctx's person, answering the reply's id.
func (w *reachWorld) replyAs(t *testing.T, ctx context.Context, post, html string) string {
	t.Helper()
	code, data, msg := w.run(ctx, postController.CreateCommentInPost, http.MethodPost, "",
		postAdapter.InputCreateOrUpdateCommentToPost{PostUuid: post, HTMLText: html})
	var created map[string]any
	_ = json.Unmarshal(data, &created)
	id, _ := created["comment_id"].(string)
	if id == "" {
		id, _ = created["uuid"].(string)
	}
	if code != http.StatusOK || id == "" {
		t.Fatalf("reply: %d %s %s", code, msg, data)
	}
	return id
}

// dmAs sends a DM to name as ctx's person, answering the message's id.
func (w *reachWorld) dmAs(t *testing.T, ctx context.Context, name, html string) string {
	t.Helper()
	code, data, msg := w.run(ctx, chatController.CreateChat, http.MethodPost, "", chatAdapter.ChatInfo{ToUuid: w.people[name].String(), TextHtml: html})
	var created map[string]any
	_ = json.Unmarshal(data, &created)
	id, _ := created["uuid"].(string)
	if code != http.StatusOK || id == "" {
		t.Fatalf("DM: %d %s", code, msg)
	}
	return id
}

func (w *reachWorld) mention(name string) string {
	return chip(w.people[name].String()+"@"+w.uids[name], name)
}

func TestOneDoorDM(t *testing.T) {
	w := newReachWorld(t)
	msg := w.dmAs(t, w.as(t, "maya", false), "jonas", "<p>lunch?</p>")
	if got, want := w.rowsFor(t, msg, len(namedRows)), toldRows("jonas", notifyBusiness.CauseDM, namedRows...); !slices.Equal(got, want) {
		t.Errorf("rows %v, want %v", got, want)
	}
	if got := w.pushedFor(t, msg, []string{"jonas"}); !slices.Equal(got, []string{"jonas"}) {
		t.Errorf("pushed %v, want Jonas", got)
	}
}

func TestOneDoorMention(t *testing.T) {
	w := newReachWorld(t)
	post := w.postAs(t, w.as(t, "maya", false), postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>" + w.mention("jonas") + " the deck?</p>"})
	if got, want := rowsOf(w.rowsFor(t, post, len(namedRows)), "jonas"), toldRows("jonas", notifyBusiness.CauseMention, namedRows...); !slices.Equal(got, want) {
		t.Errorf("Jonas's rows %v, want %v", got, want)
	}
}

func TestOneDoorReplyToYou(t *testing.T) {
	w := newReachWorld(t)
	asked := w.postAs(t, w.as(t, "jonas", false), postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>who has the deck?</p>"})
	answer := w.postAs(t, w.as(t, "maya", false), postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>I do</p>", ReplyToUUID: asked})
	if got, want := rowsOf(w.rowsFor(t, answer, len(namedRows)), "jonas"), toldRows("jonas", notifyBusiness.CauseReplyToYou, namedRows...); !slices.Equal(got, want) {
		t.Errorf("Jonas's rows %v, want %v", got, want)
	}
}

func TestOneDoorThreadReply(t *testing.T) {
	w := newReachWorld(t)
	post := w.postAs(t, w.as(t, "jonas", false), postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>the plan</p>"})
	w.replyAs(t, w.as(t, "ada", false), post, "<p>looks good</p>")
	time.Sleep(time.Second) // Ada's reply's notices are out of the way
	reply := w.replyAs(t, w.as(t, "maya", false), post, "<p>agreed</p>")
	rows := w.rowsFor(t, reply, len(namedRows)+len(loudRows))
	if got, want := rowsOf(rows, "ada"), toldRows("ada", notifyBusiness.CauseThreadReply, loudRows...); !slices.Equal(got, want) {
		t.Errorf("Ada's rows %v, want %v (she replied in the thread)", got, want)
	}
	if got, want := rowsOf(rows, "jonas"), toldRows("jonas", notifyBusiness.CauseReplyToYou, namedRows...); !slices.Equal(got, want) {
		t.Errorf("Jonas's rows %v, want %v (his post's thread)", got, want)
	}
	if got := rowsOf(rows, "sam"); len(got) != 0 {
		t.Errorf("Sam, in #launch but not the thread, has rows %v", got)
	}
}

func TestOneDoorTeam(t *testing.T) {
	w := newReachWorld(t)
	post := w.postAs(t, w.as(t, "maya", false), postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>" + chip("group:"+w.team.String(), "design") + " review?</p>"})
	rows := w.rowsFor(t, post, len(loudRows))
	if got, want := rowsOf(rows, "ada"), toldRows("ada", notifyBusiness.CauseTeam, loudRows...); !slices.Equal(got, want) {
		t.Errorf("Ada's rows %v, want %v", got, want)
	}
	if got := rowsOf(rows, "zed"); len(got) != 0 {
		t.Errorf("Zed, of the team but not in #launch, has rows %v", got)
	}
}

func TestOneDoorKeyword(t *testing.T) {
	w := newReachWorld(t)
	ada := w.as(t, "ada", false)
	if code, _, msg := w.run(ada, conversationController.PutMyKeywords, http.MethodPut, "", map[string]any{"keywords": []string{"budget"}}); code != http.StatusOK {
		t.Fatalf("Ada's keywords: %d %s", code, msg)
	}
	post := w.postAs(t, w.as(t, "maya", false), postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>The budget is in</p>"})
	if got, want := rowsOf(w.rowsFor(t, post, len(loudRows)), "ada"), toldRows("ada", notifyBusiness.CauseKeyword, loudRows...); !slices.Equal(got, want) {
		t.Errorf("Ada's rows %v, want %v", got, want)
	}
}

func TestOneDoorBroadcast(t *testing.T) {
	w := newReachWorld(t)
	post := w.postAs(t, w.as(t, "maya", false), postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>" + chip("broadcast:channel", "channel") + " moved to 10</p>"})
	if got, want := rowsOf(w.rowsFor(t, post, len(loudRows)), "jonas"), toldRows("jonas", notifyBusiness.CauseBroadcast, loudRows...); !slices.Equal(got, want) {
		t.Errorf("Jonas's rows %v, want %v", got, want)
	}
}

func TestOneDoorAllPosts(t *testing.T) {
	w := newReachWorld(t)
	post := w.postAs(t, w.as(t, "jonas", false), postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>standup in 5</p>"})
	rows := w.rowsFor(t, post, 3)
	if got, want := rows, toldRows("maya", notifyBusiness.CauseAllPosts, "desktop", "push", "sound"); !slices.Equal(got, want) {
		t.Errorf("rows %v, want %v (Maya alone hears everything in #launch)", got, want)
	}
}

func TestOneDoorReaction(t *testing.T) {
	w := newReachWorld(t)
	msg := w.dmAs(t, w.as(t, "maya", false), "jonas", "<p>lunch?</p>")
	w.rowsFor(t, msg, len(namedRows))
	if code, _, m := w.run(w.as(t, "jonas", false), chatController.CreateOrUpdateChatReaction, http.MethodPost, "",
		chatAdapter.InputUpdateReactionForChat{Uuid: msg, EmojiReactionUuid: "thumbs_up"}); code != http.StatusOK {
		t.Fatalf("Jonas's reaction: %d %s", code, m)
	}
	if got, want := rowsOf(w.rowsFor(t, msg, len(namedRows)+1), "maya"), toldRows("maya", notifyBusiness.CauseReaction, "inbox"); !slices.Equal(got, want) {
		t.Errorf("Maya's rows %v, want %v (a reaction never pushes)", got, want)
	}
}

// The same event fired twice (a retry, a double send) is one row per person
// and delivery, and tells nobody twice.
func TestOneDoorADoubleFireTellsOnce(t *testing.T) {
	w := newReachWorld(t)
	msg := "7d0e5a1c-3b2f-4e6d-8c9a-0f1e2d3c4b5a"
	ev := notifyBusiness.Event{ID: "channel:" + w.channel.String() + ":" + msg, Cause: notifyBusiness.CauseMessage, Actor: w.people["jonas"].String(),
		Place: notifyBusiness.Place{Kind: notifyBusiness.PlaceChannel, ID: w.channel.String()}, MessageID: msg,
		Push: map[string]string{firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID: msg, firebaseInit.FIREBASE_PUSH_DATA_BODY: "standup"}}
	first := notifyBusiness.Tell(context.Background(), ev)
	second := notifyBusiness.Tell(context.Background(), ev)
	if len(first) != 1 || len(second) != 0 {
		t.Errorf("first fire told %+v, second %+v; want Maya once", first, second)
	}
	if got, want := w.rowsFor(t, msg, 3), toldRows("maya", notifyBusiness.CauseAllPosts, "desktop", "push", "sound"); !slices.Equal(got, want) {
		t.Errorf("rows %v, want %v", got, want)
	}
	w.mu.Lock()
	n := 0
	for _, p := range w.pushes {
		if p.data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] == msg {
			n++
		}
	}
	w.mu.Unlock()
	if n != 1 {
		t.Errorf("%d pushes, want 1", n)
	}
}

// pause pauses name's notices for an hour: what they'd be pushed is held.
func (w *reachWorld) pause(t *testing.T, name string) {
	t.Helper()
	if _, err := w.env.PG.Exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token, notifications_paused_until)
		VALUES ($1, $2, NOW() + interval '1 hour')`, w.people[name], "unsub-"+name); err != nil {
		t.Fatal(err)
	}
}

// retracts deletes a message (del, through its route) and checks its
// notices are retracted: of before, its rows (one sent and one held among
// them at least), what was sent is marked retracted and what was held
// dropped, never sent; every row stays.
func (w *reachWorld) retracts(t *testing.T, messageID string, before []string, del func() (int, string)) {
	t.Helper()
	if !slices.ContainsFunc(before, func(r string) bool { return strings.HasSuffix(r, " sent") }) ||
		!slices.ContainsFunc(before, func(r string) bool { return strings.HasSuffix(r, " held") }) {
		t.Fatalf("rows before the delete %v, want one sent and one held at least", before)
	}
	if code, msg := del(); code != http.StatusOK {
		t.Fatalf("the delete: %d %s", code, msg)
	}
	var after []string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if after = w.rowsFor(t, messageID, 0); !slices.ContainsFunc(after, func(r string) bool { return strings.HasSuffix(r, " sent") || strings.HasSuffix(r, " held") }) {
			break
		}
	}
	if len(after) != len(before) {
		t.Errorf("%d rows after the delete, %d before: a retract keeps every row", len(after), len(before))
	}
	for i, r := range before {
		want := strings.TrimSuffix(strings.TrimSuffix(r, " sent"), " held")
		if strings.HasSuffix(r, " held") {
			want += " dropped"
		} else {
			want += " retracted"
		}
		if i < len(after) && after[i] != want {
			t.Errorf("after the delete %q, want %q", after[i], want)
		}
	}
}

// Deleting a message (soft) retracts its notices: what was sent is marked
// retracted, what was held (Ada is paused) dropped, never sent; every row
// stays.
func TestOneDoorASoftDeleteRetracts(t *testing.T) {
	w := newReachWorld(t)
	w.pause(t, "ada")
	maya := w.as(t, "maya", false)
	post := w.postAs(t, maya, postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>" + w.mention("jonas") + " " + w.mention("ada") + " the deck</p>"})
	before := w.rowsFor(t, post, 2*len(namedRows))
	if got, want := rowsOf(before, "ada"), []string{"ada badge chat.mention sent", "ada desktop chat.mention held", "ada email chat.mention held",
		"ada inbox chat.mention sent", "ada push chat.mention held", "ada sound chat.mention held"}; !slices.Equal(got, want) {
		t.Fatalf("Ada's rows before %v, want %v (paused: push held)", got, want)
	}
	w.retracts(t, post, before, func() (int, string) {
		code, _, msg := w.run(maya, postController.DeletePost, http.MethodDelete, "", postAdapter.InputCreateOrUpdatePostInfo{Uuid: post, ChannelUuid: w.channel.String()})
		return code, msg
	})
}

// Deleting a DM (soft) retracts its notices, as deleting a post does.
func TestOneDoorDeletingADMRetracts(t *testing.T) {
	w := newReachWorld(t)
	w.pause(t, "ada")
	maya := w.as(t, "maya", false)
	msg := w.dmAs(t, maya, "ada", "<p>the deck</p>")
	w.retracts(t, msg, w.rowsFor(t, msg, len(namedRows)), func() (int, string) {
		code, _, m := w.run(maya, chatController.DeleteChat, http.MethodPost, "", chatAdapter.ChatInfo{Uuid: msg})
		return code, m
	})
}

// Deleting a reply in a DM's thread (soft) retracts its notices.
func TestOneDoorDeletingADMReplyRetracts(t *testing.T) {
	w := newReachWorld(t)
	w.pause(t, "ada")
	maya := w.as(t, "maya", false)
	lunch := w.dmAs(t, w.as(t, "ada", false), "maya", "<p>lunch?</p>")
	code, data, m := w.run(maya, chatController.CreateChatComment, http.MethodPost, "", chatAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: lunch, HTMLText: "<p>12:30?</p>"})
	var created map[string]any
	_ = json.Unmarshal(data, &created)
	reply, _ := created["comment_id"].(string)
	if code != http.StatusOK || reply == "" {
		t.Fatalf("Maya's reply: %d %s %s", code, m, data)
	}
	w.retracts(t, reply, w.rowsFor(t, reply, len(loudRows)), func() (int, string) {
		code, _, m := w.run(maya, chatController.DeleteCommentOnChat, http.MethodPost, "", chatAdapter.InputCreateOrUpdateCommentToChat{Uuid: reply})
		return code, m
	})
}

// Deleting a reply in a channel post's thread (soft) retracts its notices.
func TestOneDoorDeletingAThreadReplyRetracts(t *testing.T) {
	w := newReachWorld(t)
	w.pause(t, "ada")
	maya := w.as(t, "maya", false)
	post := w.postAs(t, w.as(t, "ada", false), postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>the plan</p>"})
	reply := w.replyAs(t, maya, post, "<p>agreed</p>")
	w.retracts(t, reply, w.rowsFor(t, reply, len(namedRows)), func() (int, string) {
		code, _, m := w.run(maya, postController.DeleteCommentInPost, http.MethodPost, "", postAdapter.InputCreateOrUpdateCommentToPost{Uuid: reply})
		return code, m
	})
}

// react reacts to the DM msg with emoji as name, answering the reaction's
// node.
func (w *reachWorld) react(t *testing.T, name, msg, emoji string) string {
	t.Helper()
	code, data, m := w.run(w.as(t, name, false), chatController.CreateOrUpdateChatReaction, http.MethodPost, "",
		chatAdapter.InputUpdateReactionForChat{Uuid: msg, EmojiReactionUuid: emoji})
	var r chatAdapter.InputUpdateReactionForChat
	_ = json.Unmarshal(data, &r)
	if code != http.StatusOK || r.ReactionDgraphUid == "" {
		t.Fatalf("%s's %s: %d %s", name, emoji, code, m)
	}
	return r.ReactionDgraphUid
}

// unreact takes name's reaction (its node) to the DM msg back.
func (w *reachWorld) unreact(t *testing.T, name, msg, reaction string) {
	t.Helper()
	if code, _, m := w.run(w.as(t, name, false), chatController.DeleteChatReaction, http.MethodPost, "",
		chatAdapter.InputUpdateReactionForChat{Uuid: msg, ReactionDgraphUid: reaction}); code != http.StatusOK {
		t.Fatalf("%s takes a reaction back: %d %s", name, code, m)
	}
}

// reactionItems is how many live reaction items name's Activity was sent,
// once at least want are (or 10 s), and a moment more for any that
// shouldn't be.
func (w *reachWorld) reactionItems(mq *integration.MqttRecorder, name string, want int) int {
	topic := helpers.GetMqttTopicForUserActivity(w.people[name].String())
	count := func() int {
		n := 0
		for _, data := range mq.Messages(topic, mqttStruct.MESSAGE_ACTIVITY) {
			var m struct {
				Activity struct {
					ActivityType string `json:"activity_type"`
				} `json:"activity"`
			}
			if json.Unmarshal(data, &m) == nil && m.Activity.ActivityType == mqttStruct.MESSAGE_ACTIVITY_REACTION {
				n++
			}
		}
		return n
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && count() < want; {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	return count()
}

// Each reaction someone makes to a message, another emoji after taking one
// back included, is its own notice to its author: a row, and a live item on
// their Activity, as each was before A4 H2. The same emoji again is the same
// notice, recorded once, though its live item goes, as it always did.
func TestOneDoorEachReactionIsToldItsAuthor(t *testing.T) {
	w := newReachWorld(t)
	mq := integration.RecordMqtt(t)
	msg := w.dmAs(t, w.as(t, "maya", false), "jonas", "<p>lunch?</p>")
	w.rowsFor(t, msg, len(namedRows))
	w.unreact(t, "jonas", msg, w.react(t, "jonas", msg, "thumbs_up"))
	heart := w.react(t, "jonas", msg, "heart")
	reacted := toldRows("maya", notifyBusiness.CauseReaction, "inbox", "inbox")
	if got := rowsOf(w.rowsFor(t, msg, len(namedRows)+2), "maya"); !slices.Equal(got, reacted) {
		t.Errorf("Maya's rows %v, want %v: one an emoji", got, reacted)
	}
	if got := w.reactionItems(mq, "maya", 2); got != 2 {
		t.Errorf("Maya's Activity got %d live reaction items, want 2: one an emoji", got)
	}
	w.unreact(t, "jonas", msg, heart)
	w.react(t, "jonas", msg, "heart")
	if got := w.reactionItems(mq, "maya", 3); got != 3 {
		t.Errorf("Maya's Activity got %d live reaction items, want 3: one a reaction", got)
	}
	if got := rowsOf(w.rowsFor(t, msg, 0), "maya"); !slices.Equal(got, reacted) {
		t.Errorf("Maya's rows %v, want %v: the same emoji again is recorded once", got, reacted)
	}
}

// An author on "Nothing" for the conversation still gets a reaction's live
// item on their Activity, as before A4 H2 (the setting gated only the push):
// no row, as the router's Inbox decision has it, and never a push (D2).
func TestOneDoorAReactionReachesAnAuthorOnNothing(t *testing.T) {
	w := newReachWorld(t)
	mq := integration.RecordMqtt(t)
	msg := w.dmAs(t, w.as(t, "maya", false), "jonas", "<p>lunch?</p>")
	w.rowsFor(t, msg, len(namedRows))
	res, err := w.env.PG.Exec(`UPDATE users_chat_notification SET notification_type = 'block' WHERE user_id = $1 AND grp_id = $2`,
		w.people["maya"], helpers.GetGroupingId(w.people["maya"].String(), w.people["jonas"].String()))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("Maya on Nothing: %d rows changed, want 1", n)
	}
	w.react(t, "jonas", msg, "thumbs_up")
	if got := w.reactionItems(mq, "maya", 1); got != 1 {
		t.Errorf("Maya's Activity got %d live reaction items, want 1: she is in the conversation", got)
	}
	if got := rowsOf(w.rowsFor(t, msg, 0), "maya"); len(got) != 0 {
		t.Errorf("Maya, on Nothing, has rows %v", got)
	}
}

// An edit that adds a mention goes through the one door (V-843), as a send's
// mention does: whom it newly names, of those who may hear of it there, gets
// one row per delivery with cause chat.mention and the message's id, and
// Why answers them; whom the message named before isn't told again; a second
// identical edit, or a remove and a re-add, tells nobody twice; and deleting
// the message retracts the edit's rows (sent -> retracted, held -> dropped)
// as it does the send's. What they see is kept: the push's type and "<who>
// mentioned you in <where>", and one Inbox notice.

// whyOf is name's answer to GET /notify/why/{messageID}: the events it
// recalls for them.
func (w *reachWorld) whyOf(t *testing.T, name, messageID string) []notifyBusiness.Trace {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/notify/why/{message_id}", notificationController.WhyAmISeeingThis)
	var info userModels.UserInfo
	info.UserPostgresInfo.Id = w.people[name]
	req := httptest.NewRequest(http.MethodGet, "/notify/why/"+messageID, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, info)))
	if rec.Code != http.StatusOK {
		t.Errorf("Why %s for %s: %d %s", messageID, name, rec.Code, rec.Body)
		return nil
	}
	var got struct {
		Events []notifyBusiness.Trace `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.Events
}

// groupAs starts a group of ctx's person and names with html, answering the
// message's id.
func (w *reachWorld) groupAs(t *testing.T, ctx context.Context, html string, names ...string) string {
	t.Helper()
	var members []string
	for _, n := range names {
		members = append(members, w.uids[n])
	}
	code, data, msg := w.run(ctx, chatController.CreateGroupChat, http.MethodPost, "", chatAdapter.ChatInfo{Participants: members, TextHtml: html})
	var created map[string]any
	_ = json.Unmarshal(data, &created)
	id, _ := created["uuid"].(string)
	if code != http.StatusOK || id == "" {
		t.Fatalf("group: %d %s %s", code, msg, data)
	}
	return id
}

// without is rows less each of less, once per copy. Pure.
func without(rows, less []string) []string {
	out := slices.Clone(rows)
	for _, r := range less {
		if i := slices.Index(out, r); i >= 0 {
			out = slices.Delete(out, i, i+1)
		}
	}
	return out
}

// mentionEdit is a message (msg, whose send named Jonas) and how its edits
// reach people: edit saves html through its route; the push names thread
// (the message, or a reply's post) and is of type kind, titled "… mentioned
// you in where"; the Inbox notice's data has noticeKey msg's id (noticeID);
// del deletes the message through its route.
type mentionEdit struct {
	msg, thread, kind, where, noticeKey, noticeID string
	// saveKind is what holds the mention node (notificationBusiness.MentionSave.Kind).
	saveKind string
	edit     func(html string)
	del      func() (int, string)
}

// addsMentions checks e: Maya's edit names Ada (paused) and Sam beside
// Jonas, and tells the two of them through the door.
func (w *reachWorld) addsMentions(t *testing.T, e mentionEdit) {
	t.Helper()
	helpers.SettleBackground(10 * time.Second)
	sent := w.rowsFor(t, e.msg, 0)
	jonas := rowsOf(sent, "jonas")
	if len(jonas) == 0 {
		t.Fatalf("the send told Jonas nothing: %v", sent)
	}
	added := "<p>" + w.mention("jonas") + " " + w.mention("ada") + " " + w.mention("sam") + " the deck?</p>"
	e.edit(added)
	want := append(toldRows("sam", notifyBusiness.CauseMention, namedRows...), "ada badge chat.mention sent", "ada desktop chat.mention held",
		"ada email chat.mention held", "ada inbox chat.mention sent", "ada push chat.mention held", "ada sound chat.mention held")
	sort.Strings(want)
	edited := w.rowsFor(t, e.msg, len(sent)+len(want))
	if got := without(edited, sent); !slices.Equal(got, want) {
		t.Fatalf("the edit's rows %v, want %v (one per delivery, chat.mention; Ada is paused)", got, want)
	}
	if got := rowsOf(edited, "jonas"); !slices.Equal(got, jonas) {
		t.Errorf("Jonas, named before, has rows %v after the edit, %v before", got, jonas)
	}

	// Why answers Sam: the mention, told by every delivery now.
	var why *notifyBusiness.Trace
	for _, tr := range w.whyOf(t, "sam", e.msg) {
		if tr.Decision != nil && tr.Decision.Cause == notifyBusiness.CauseMention {
			why = &tr
		}
	}
	if why == nil || len(why.Decision.To) != len(namedRows) || len(why.Decision.Held) != 0 {
		t.Errorf("Why answers Sam %+v; want the mention, told by its %d deliveries", why, len(namedRows))
	}

	// What Sam sees is kept: one push of the mention, as before, and one
	// Inbox notice.
	pushed := func() []string {
		w.mu.Lock()
		defer w.mu.Unlock()
		var titles []string
		for _, p := range w.pushes {
			if p.data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] == e.thread && slices.Contains(p.tokens, "tok-sam") &&
				strings.Contains(p.data[firebaseInit.FIREBASE_PUSH_DATA_TITLE], " mentioned you in ") {
				titles = append(titles, p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE]+" "+p.data[firebaseInit.FIREBASE_PUSH_DATA_TITLE])
			}
		}
		return titles
	}
	notices := func() int {
		var n int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM resource_notices WHERE user_uuid = $1 AND kind = 'message.mentioned' AND data->>$2 = $3`, w.people["sam"], e.noticeKey, e.noticeID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	once := func(when string) {
		t.Helper()
		helpers.SettleBackground(10 * time.Second)
		if got, want := pushed(), []string{e.kind + " maya mentioned you in " + e.where}; !slices.Equal(got, want) {
			t.Errorf("%s: Sam was pushed %q, want %q", when, got, want)
		}
		if n := notices(); n != 1 {
			t.Errorf("%s: Sam holds %d Inbox notices of the edit, want 1", when, n)
		}
	}
	once("after the edit")

	// The same edit again, and a remove and a re-add, tell nobody twice.
	e.edit(added)
	if got := w.rowsFor(t, e.msg, len(edited)); !slices.Equal(got, edited) {
		t.Errorf("a second identical edit: rows %v, want %v", got, edited)
	}
	e.edit("<p>" + w.mention("jonas") + " the deck?</p>")
	helpers.SettleBackground(10 * time.Second)
	e.edit(added)
	if got := w.rowsFor(t, e.msg, len(edited)); !slices.Equal(got, edited) {
		t.Errorf("a remove and a re-add: rows %v, want %v (once ever)", got, edited)
	}
	once("after a re-add")

	// Deleting the message retracts the edit's rows with the send's, and takes
	// its Inbox notice back: it leaves Sam's Inbox and badge, the row kept
	// (soft), and the graph's mention of it stays left out of the feed.
	shown := func() (n int) {
		list, err := followModels.NoticesFor(context.Background(), w.people["sam"], time.Now().Add(time.Minute), 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range list {
			if it.Kind == "message.mentioned" && it.Data[e.noticeKey] == e.noticeID {
				n++
			}
		}
		return n
	}
	badge := func() uint64 {
		c, err := followModels.CountNoticesSince(context.Background(), w.people["sam"], time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	before := badge()
	w.retracts(t, e.msg, edited, e.del)
	helpers.SettleBackground(10 * time.Second)
	if n := shown(); n != 0 {
		t.Errorf("after the delete: Sam's Inbox shows %d notices of the edit, want 0", n)
	}
	if after := badge(); after != before-1 {
		t.Errorf("after the delete: Sam's badge counts %d notices, want %d (the edit's taken back)", after, before-1)
	}
	if n := notices(); n != 1 {
		t.Errorf("after the delete: Sam has %d notice rows of the edit, want 1 (kept: every delete is soft)", n)
	}
	key := followModels.MentionKey(e.saveKind, e.msg) + "|" + w.people["sam"].String()
	if held, err := followModels.HeldNotices(context.Background(), []string{key}); err != nil || !held[key] {
		t.Errorf("after the delete: the edit's notice isn't held (%v, %v), so the graph's mention would list it again", held, err)
	}
}

// (a) A channel post's edit.
func TestOneDoorAPostsEditTellsWhomItAdds(t *testing.T) {
	w := newReachWorld(t)
	w.pause(t, "ada")
	maya := w.as(t, "maya", false)
	post := w.postAs(t, maya, postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>" + w.mention("jonas") + " the deck?</p>"})
	w.addsMentions(t, mentionEdit{msg: post, thread: post, kind: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHANNEL, where: "#launch", noticeKey: "post_id", noticeID: post, saveKind: "post",
		edit: func(html string) {
			if code, _, msg := w.run(maya, postController.UpdatePost, http.MethodPut, "", postAdapter.InputCreateOrUpdatePostInfo{Uuid: post, ChannelUuid: w.channel.String(), HTMLText: html}); code != http.StatusOK {
				t.Fatalf("the edit: %d %s", code, msg)
			}
		},
		del: func() (int, string) {
			code, _, msg := w.run(maya, postController.DeletePost, http.MethodDelete, "", postAdapter.InputCreateOrUpdatePostInfo{Uuid: post, ChannelUuid: w.channel.String()})
			return code, msg
		}})
}

// (b) A group message's edit (a DM's goes the same way: one function).
func TestOneDoorAGroupMessagesEditTellsWhomItAdds(t *testing.T) {
	w := newReachWorld(t)
	w.pause(t, "ada")
	maya := w.as(t, "maya", false)
	msg := w.groupAs(t, maya, "<p>"+w.mention("jonas")+" the deck?</p>", "maya", "jonas", "ada", "sam")
	var ids []string
	for _, n := range []string{"maya", "jonas", "ada", "sam"} {
		ids = append(ids, w.people[n].String())
	}
	grp, _ := helpers.GenerateGroupID(ids)
	w.addsMentions(t, mentionEdit{msg: msg, thread: msg, kind: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT, where: "a conversation", noticeKey: "message_id", noticeID: msg, saveKind: "chat",
		edit: func(html string) {
			// A group's message is edited through the group's route (a DM's
			// through updateChat, which answers 404 for a group's).
			if code, _, m := w.run(maya, chatController.UpdateChatInGroup, http.MethodPost, "", chatAdapter.ChatInfo{Uuid: msg, GrpUuid: grp, TextHtml: html}); code != http.StatusOK {
				t.Fatalf("the edit: %d %s", code, m)
			}
		},
		del: func() (int, string) {
			code, _, m := w.run(maya, chatController.DeleteChat, http.MethodPost, "", chatAdapter.ChatInfo{Uuid: msg})
			return code, m
		}})
}

// (c) A reply's edit in a channel post's thread: its delete retracts by the
// reply's id (business/Post DeleteCommentInPost), so the edit's rows carry it.
func TestOneDoorAThreadReplysEditTellsWhomItAdds(t *testing.T) {
	w := newReachWorld(t)
	w.pause(t, "ada")
	maya := w.as(t, "maya", false)
	post := w.postAs(t, w.as(t, "scout", false), postAdapter.InputCreateOrUpdatePostInfo{HTMLText: "<p>the plan</p>"})
	reply := w.replyAs(t, maya, post, "<p>"+w.mention("jonas")+" the deck?</p>")
	w.addsMentions(t, mentionEdit{msg: reply, thread: post, kind: firebaseInit.FIREBASE_PUSH_DATA_TYPE_POST_COMMENT, where: "#launch", noticeKey: "comment_uuid", noticeID: reply, saveKind: "comment",
		edit: func(html string) {
			if code, _, m := w.run(maya, postController.UpdateCommentInPost, http.MethodPost, "", postAdapter.InputCreateOrUpdateCommentToPost{Uuid: reply, PostUuid: post, HTMLText: html}); code != http.StatusOK {
				t.Fatalf("the edit: %d %s", code, m)
			}
		},
		del: func() (int, string) {
			code, _, m := w.run(maya, postController.DeleteCommentInPost, http.MethodPost, "", postAdapter.InputCreateOrUpdateCommentToPost{Uuid: reply})
			return code, m
		}})
}
