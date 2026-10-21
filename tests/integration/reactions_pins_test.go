//go:build integration
// +build integration

package integration_test

// Reactions like Slack's and pinned messages (chat B25), against real
// Postgres 12, Redis and Dgraph, through the handlers the app calls.
//
// Reactions: a person adds several emoji to a message, each once. Adding one
// they have answers the one they have and writes nothing, however many
// times at once (the reaction key's @upsert). Taking one back takes back
// every copy of it (adds from before this made a node each time), and only
// theirs. An app from before several reactions still changes its one
// reaction. The caps refuse in words. A DM's author hears of someone's
// reactions once. Replies in a thread, in a DM's thread and on a task are
// reacted to and taken back the same way.
//
// Pins: whoever posts in a conversation pins and unpins there, once; the pin
// names who pinned it, a quiet line says so, and the list shows the message
// as it is now (a deleted one's pin goes). An announcement channel's members,
// a public channel's readers and other conversations' messages are refused
// in words; a private channel's pins are nobody else's business; a
// conversation holds 100.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestReactionsLikeSlacks|TestATaskCommentsReactionComesOff|TestPinnedMessages' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	taskAdapter "github.com/akashc777/OneCamp/adapter/Task"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	conversationController "github.com/akashc777/OneCamp/controllers/Conversation"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// b25World is three people, Maya, Jonas and Sam, in a server with stores.
type b25World struct {
	env    *integration.Env
	dg     *integration.DgraphEnv
	people map[string]uuid.UUID
	uids   map[string]string
}

func newB25World(t *testing.T) *b25World {
	t.Helper()
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	w := &b25World{env: env, dg: dg, people: map[string]uuid.UUID{}, uids: map[string]string{}}
	for _, name := range []string{"maya", "jonas", "sam"} {
		id := uuid.New()
		w.people[name] = id
		for _, q := range []string{
			`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`,
			`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-' || $2)`,
		} {
			if _, err := env.PG.Exec(q, id, name); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		got := dg.Mutate(t, map[string]any{"uid": "_:u", "dgraph.type": "User", "user_uuid": id.String(), "user_name": name,
			"user_deleted_at": "0001-01-01T00:00:00Z"})
		w.uids[name] = got["u"]
	}
	return w
}

func (w *b25World) as(t *testing.T, name string) context.Context {
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

// call runs handler as name with body and the route's {kind} and {id}, and
// answers its status and its data.
func (w *b25World) call(t *testing.T, name string, handler http.HandlerFunc, method string, body any, params ...string) (int, json.RawMessage, string) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	ctx := w.as(t, name)
	if len(params) > 0 {
		rc := chi.NewRouteContext()
		for i := 0; i+1 < len(params); i += 2 {
			rc.URLParams.Add(params[i], params[i+1])
		}
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rc)
	}
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(method, "/", bytes.NewReader(raw)).WithContext(ctx))
	var out struct {
		Data json.RawMessage `json:"data"`
		Msg  string          `json:"msg"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.Data, out.Msg
}

// reactionsOn is what the graph holds through edge on the node uuidPred
// names: "name:emoji" per reaction, sorted.
func (w *b25World) reactionsOn(t *testing.T, uuidPred, edge, id string) []string {
	t.Helper()
	var got struct {
		P []struct {
			R []struct {
				Emoji string `json:"reaction_emoji_id"`
				By    struct {
					UUID string `json:"user_uuid"`
				} `json:"reaction_added_by"`
			} `json:"r"`
		}
	}
	q := fmt.Sprintf(`{ p(func: eq(%s, %q)) { r: %s { reaction_emoji_id reaction_added_by { user_uuid } } } }`, uuidPred, id, edge)
	if err := json.Unmarshal(w.dg.Query(t, q), &got); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for n, id := range w.people {
		names[id.String()] = n
	}
	var out []string
	for _, p := range got.P {
		for _, r := range p.R {
			out = append(out, names[r.By.UUID]+":"+r.Emoji)
		}
	}
	sort.Strings(out)
	return out
}

func reactionUID(t *testing.T, code int, data json.RawMessage, msg string) string {
	t.Helper()
	var r struct {
		UID string `json:"reaction_dgraph_id"`
	}
	if code != http.StatusOK || json.Unmarshal(data, &r) != nil || r.UID == "" {
		t.Fatalf("reacting: %d %s %s", code, data, msg)
	}
	return r.UID
}

const liveAt = "0001-01-01T00:00:00Z"

// channel puts a channel in the graph with members, managers, a policy and
// posts (uuid → its author), and answers the posts' graph uids by uuid.
func (w *b25World) channel(t *testing.T, id, name string, private bool, policy string, members, managers []string, posts map[string]string) {
	t.Helper()
	var mem, mods, ps []map[string]any
	for _, n := range members {
		mem = append(mem, map[string]any{"uid": w.uids[n]})
	}
	for _, n := range managers {
		mods = append(mods, map[string]any{"uid": w.uids[n]})
	}
	i := 0
	for postID, by := range posts {
		i++
		kind := ""
		if by == "system" {
			kind, by = "system", "jonas"
		}
		ps = append(ps, map[string]any{"uid": fmt.Sprintf("_:p%d", i), "dgraph.type": "Post", "post_uuid": postID,
			"post_text": "<p>" + postID[:8] + "</p>", "post_by": map[string]any{"uid": w.uids[by]}, "post_channel": map[string]any{"uid": "_:ch"},
			"post_created_at": time.Now().Add(-time.Duration(i) * time.Minute).UTC().Format(time.RFC3339), "post_deleted_at": liveAt, "post_kind": kind})
	}
	ch := map[string]any{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": id, "ch_name": name, "ch_private": private,
		"ch_deleted_at": liveAt, "ch_members": mem, "ch_posts": ps}
	if len(mods) > 0 {
		ch["ch_moderators"] = mods
	}
	if policy != "" {
		ch["ch_post_policy"] = policy
	}
	w.dg.Mutate(t, ch)
}

func TestReactionsLikeSlacks(t *testing.T) {
	w := newB25World(t)
	var mu sync.Mutex
	var pushes []sentPush
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		mu.Lock()
		pushes = append(pushes, sentPush{data, tokens})
		mu.Unlock()
	})()

	launch, plan := uuid.NewString(), uuid.NewString()
	w.channel(t, launch, "launch", false, "", []string{"maya", "jonas"}, nil, map[string]string{plan: "jonas"})
	react := func(name, emoji, old string) (int, json.RawMessage, string) {
		return w.call(t, name, postController.CreateOrUpdateReaction, http.MethodPost, postAdapter.InputUpdateReactionForPost{Uuid: plan, EmojiUuid: emoji, ReactionDgraphUid: old})
	}
	onPlan := func() []string { return w.reactionsOn(t, "post_uuid", "post_reactions", plan) }
	uid := func(code int, data json.RawMessage, msg string) string { return reactionUID(t, code, data, msg) }

	t.Run("several emoji, each once", func(t *testing.T) {
		thumbs := uid(react("maya", "+1", ""))
		tada := uid(react("maya", "tada", ""))
		if thumbs == tada {
			t.Fatalf("a second emoji replaced the first: both %s", tada)
		}
		if again := uid(react("maya", "+1", "")); again != thumbs {
			t.Fatalf("the same emoji again answered %s, want her %s", again, thumbs)
		}
		uid(react("jonas", "+1", ""))
		if got := onPlan(); !slices.Equal(got, []string{"jonas:+1", "maya:+1", "maya:tada"}) {
			t.Fatalf("holds %v", got)
		}
	})

	t.Run("many adds of one emoji at once make one reaction", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				react("maya", "rocket", "")
			}()
		}
		wg.Wait()
		n := 0
		for _, r := range onPlan() {
			if r == "maya:rocket" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("eight adds at once made %d rockets of Maya's, want one: %v", n, onPlan())
		}
	})

	t.Run("taking one back takes back every copy, and only yours", func(t *testing.T) {
		// A copy from before: an add made a node each time.
		posts := w.dg.Query(t, fmt.Sprintf(`{ p(func: eq(post_uuid, %q)) { uid } }`, plan))
		var got struct{ P []struct{ UID string } }
		_ = json.Unmarshal(posts, &got)
		w.dg.Mutate(t, map[string]any{"uid": got.P[0].UID, "post_reactions": []map[string]any{{"dgraph.type": "Reaction",
			"reaction_emoji_id": "+1", "reaction_added_by": map[string]any{"uid": w.uids["maya"]}, "reaction_added_at": time.Now().UTC().Format(time.RFC3339)}}})
		if got := onPlan(); !slices.Equal(got, []string{"jonas:+1", "maya:+1", "maya:+1", "maya:rocket", "maya:tada"}) {
			t.Fatalf("setup holds %v", got)
		}
		thumbs := uid(react("maya", "+1", ""))

		code, _, msg := w.call(t, "jonas", postController.DeletePostReaction, http.MethodPost, postAdapter.InputDeleteReactionForPost{Uuid: plan, ReactionDgraphUid: thumbs})
		if code != http.StatusForbidden || msg != "You can only change or remove your own reaction." {
			t.Fatalf("Jonas taking Maya's: %d %q", code, msg)
		}
		if code, _, msg := w.call(t, "maya", postController.DeletePostReaction, http.MethodPost, postAdapter.InputDeleteReactionForPost{Uuid: plan, ReactionDgraphUid: thumbs}); code != http.StatusOK {
			t.Fatalf("Maya taking hers: %d %q", code, msg)
		}
		if got := onPlan(); !slices.Equal(got, []string{"jonas:+1", "maya:rocket", "maya:tada"}) {
			t.Fatalf("after: %v, want both of Maya's 👍 gone and Jonas's kept", got)
		}
	})

	t.Run("an app from before several reactions still changes its one, and keeps its node", func(t *testing.T) {
		tada := uid(react("maya", "tada", ""))
		heart := uid(react("maya", "heart", tada))
		if heart != tada {
			t.Fatalf("the change answered %s, want the node the app holds, %s", heart, tada)
		}
		if got := onPlan(); !slices.Equal(got, []string{"jonas:+1", "maya:heart", "maya:rocket"}) {
			t.Fatalf("after the change: %v", got)
		}
		// The app takes it back by that node later.
		if code, _, msg := w.call(t, "maya", postController.DeletePostReaction, http.MethodPost, postAdapter.InputDeleteReactionForPost{Uuid: plan, ReactionDgraphUid: heart}); code != http.StatusOK {
			t.Fatalf("taking back the changed one: %d %q", code, msg)
		}
		uid(react("maya", "heart", ""))
	})

	t.Run("the caps refuse in words", func(t *testing.T) {
		for i := 3; i <= 23; i++ { // she has two
			uid(react("maya", fmt.Sprintf("e%02d", i), ""))
		}
		code, _, msg := react("maya", "eyes", "")
		if code != http.StatusConflict || msg != "You can add up to 23 reactions to one message." {
			t.Fatalf("a 24th: %d %q", code, msg)
		}
		code, _, msg = react("maya", "<b>x</b>", "")
		if code != http.StatusBadRequest || msg != "That isn't an emoji we know." {
			t.Fatalf("not an emoji: %d %q", code, msg)
		}
	})

	t.Run("a DM's author hears of someone's reactions once", func(t *testing.T) {
		_, data, _ := w.call(t, "jonas", chatController.CreateChat, http.MethodPost, chatAdapter.ChatInfo{ToUuid: w.people["maya"].String(), TextHtml: "<p>Lunch?</p>"})
		var created struct {
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(data, &created) != nil || created.UUID == "" {
			t.Fatalf("the DM: %s", data)
		}
		mu.Lock()
		pushes = nil
		mu.Unlock()
		reactChat := func(emoji string) string {
			return uid(w.call(t, "maya", chatController.CreateOrUpdateChatReaction, http.MethodPost, chatAdapter.InputUpdateReactionForChat{Uuid: created.UUID, EmojiReactionUuid: emoji}))
		}
		reactChat("+1")
		reactChat("eyes")
		reactChat("+1")
		if got := w.reactionsOn(t, "chat_uuid", "chat_reactions", created.UUID); !slices.Equal(got, []string{"maya:+1", "maya:eyes"}) {
			t.Fatalf("the DM holds %v", got)
		}
		reactionPushes := func() int {
			mu.Lock()
			defer mu.Unlock()
			n := 0
			for _, p := range pushes {
				if p.data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_REACTION {
					n++
				}
			}
			return n
		}
		waitFor(t, "the reaction's push", func() bool { return reactionPushes() > 0 })
		time.Sleep(time.Second) // a second push would be on its way by now
		if n := reactionPushes(); n != 1 {
			t.Fatalf("Jonas was pushed %d times for Maya's reactions, want once", n)
		}
	})

	// A reply's reactions are found by the reply the request names. The
	// replies' lookups didn't say which reply they had found, so taking one
	// back answered "That message isn't there any more."
	t.Run("a reply's reaction comes off as a message's does", func(t *testing.T) {
		replyID := func(code int, data json.RawMessage, msg string) string {
			t.Helper()
			var r struct {
				ID string `json:"comment_id"`
			}
			if code != http.StatusOK || json.Unmarshal(data, &r) != nil || r.ID == "" {
				t.Fatalf("the reply: %d %s %s", code, data, msg)
			}
			return r.ID
		}

		// In the channel's thread.
		reply := replyID(w.call(t, "jonas", postController.CreateCommentInPost, http.MethodPost, postAdapter.InputCreateOrUpdateCommentToPost{PostUuid: plan, HTMLText: "<p>Friday works</p>"}))
		onReply := func() []string { return w.reactionsOn(t, "comment_uuid", "comment_reactions", reply) }
		reactReply := func(emoji string) string {
			return uid(w.call(t, "maya", postController.CreateOrUpdateReactionOnCommentPost, http.MethodPost, postAdapter.InputUpdateReactionForCommentInPost{Uuid: reply, EmojiUuid: emoji}))
		}
		thumbs := reactReply("+1")
		reactReply("eyes")
		if again := reactReply("+1"); again != thumbs {
			t.Fatalf("the same emoji again on the reply answered %s, want her %s", again, thumbs)
		}
		if code, _, msg := w.call(t, "maya", postController.DeletePostCommentReaction, http.MethodPost, postAdapter.InputDeleteReactionForPostComment{Uuid: reply, ReactionDgraphUid: thumbs}); code != http.StatusOK {
			t.Fatalf("Maya taking back her 👍 on the thread's reply: %d %q", code, msg)
		}
		if got := onReply(); !slices.Equal(got, []string{"maya:eyes"}) {
			t.Fatalf("the thread's reply holds %v, want only Maya's 👀", got)
		}

		// In a DM's thread.
		_, data, msg := w.call(t, "jonas", chatController.CreateChat, http.MethodPost, chatAdapter.ChatInfo{ToUuid: w.people["maya"].String(), TextHtml: "<p>Dinner?</p>"})
		var dm struct {
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(data, &dm) != nil || dm.UUID == "" {
			t.Fatalf("the DM: %s %s", data, msg)
		}
		dmReply := replyID(w.call(t, "jonas", chatController.CreateChatComment, http.MethodPost, chatAdapter.InputCreateOrUpdateCommentToChat{ChatUuid: dm.UUID, HTMLText: "<p>At eight</p>"}))
		reactDMReply := func(emoji string) string {
			return uid(w.call(t, "maya", chatController.CreateOrUpdateChatCommentReaction, http.MethodPost, chatAdapter.InputUpdateReactionForCommentInChat{Uuid: dmReply, EmojiReactionUuid: emoji}))
		}
		heart := reactDMReply("heart")
		reactDMReply("+1")
		if code, _, msg := w.call(t, "maya", chatController.DeleteReactionOnCommentChat, http.MethodPost, chatAdapter.InputUpdateReactionForCommentInChat{Uuid: dmReply, ReactionDgraphUid: heart}); code != http.StatusOK {
			t.Fatalf("Maya taking back her ❤️ on the DM's reply: %d %q", code, msg)
		}
		if got := w.reactionsOn(t, "comment_uuid", "comment_reactions", dmReply); !slices.Equal(got, []string{"maya:+1"}) {
			t.Fatalf("the DM's reply holds %v, want only Maya's 👍", got)
		}
	})
}

// TestATaskCommentsReactionComesOff: a task comment's reactions, as a
// message's: several emoji a person, each once, and taking one back finds the
// comment the request names (its lookup didn't say which it had found).
func TestATaskCommentsReactionComesOff(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	integration.SetupRedis(t)
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	t.Cleanup(firebaseInit.ObservePushesForTest(func(map[string]string, []string) {}))

	call := func(who string, h http.HandlerFunc, body any) (int, json.RawMessage, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, w.account(who))))
		var out struct {
			Data json.RawMessage `json:"data"`
			Msg  string          `json:"msg"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Data, out.Msg
	}
	code, data, msg := call("ben", taskController.CreateTaskComment, map[string]string{"task_uuid": w.task, "task_comment_body": "<p>Looks good</p>"})
	var made struct {
		ID string `json:"comment_id"`
	}
	if code != http.StatusOK || json.Unmarshal(data, &made) != nil || made.ID == "" {
		t.Fatalf("Ben's comment: %d %s %s", code, data, msg)
	}
	react := func(emoji string) string {
		code, data, msg := call("ana", taskController.CreateOrUpdateReactionOnCommentTask, taskAdapter.InputUpdateReactionForCommentInTask{Uuid: made.ID, EmojiUuid: emoji})
		return reactionUID(t, code, data, msg)
	}
	tada := react("tada")
	react("eyes")
	if again := react("tada"); again != tada {
		t.Fatalf("the same emoji again answered %s, want her %s", again, tada)
	}
	if code, _, msg := call("ana", taskController.DeleteTaskCommentReaction, taskAdapter.InputDeleteReactionForTaskComment{Uuid: made.ID, ReactionDgraphUid: tada}); code != http.StatusOK {
		t.Fatalf("Ana taking back her 🎉: %d %q", code, msg)
	}
	var got struct {
		P []struct {
			R []struct {
				Emoji string `json:"reaction_emoji_id"`
			} `json:"r"`
		}
	}
	if err := json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ p(func: eq(comment_uuid, %q)) { r: comment_reactions { reaction_emoji_id } } }`, made.ID)), &got); err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, p := range got.P {
		for _, r := range p.R {
			left = append(left, r.Emoji)
		}
	}
	if !slices.Equal(left, []string{"eyes"}) {
		t.Fatalf("the comment holds %v, want only Ana's 👀", left)
	}
}

// pinsAnswer is GET or POST /conversations/{kind}/{id}/pins's data.
type pinsAnswer struct {
	Pins []struct {
		MessageID string `json:"message_id"`
		PinnedBy  struct {
			UUID string `json:"user_uuid"`
			Name string `json:"user_name"`
		} `json:"pinned_by"`
		Message struct {
			HTML string `json:"html"`
		} `json:"message"`
	} `json:"pins"`
	CanPin bool `json:"can_pin"`
	Max    int  `json:"max"`
}

func TestPinnedMessages(t *testing.T) {
	w := newB25World(t)
	launch, news, secret := uuid.NewString(), uuid.NewString(), uuid.NewString()
	plan, recap, line, notice, hush := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	w.channel(t, launch, "launch", false, "", []string{"maya", "jonas"}, nil, map[string]string{plan: "jonas", recap: "maya", line: "system"})
	w.channel(t, news, "news", false, "admins_only", []string{"maya", "jonas"}, []string{"jonas"}, map[string]string{notice: "jonas"})
	w.channel(t, secret, "secret", true, "", []string{"maya", "jonas"}, nil, map[string]string{hush: "maya"})

	pins := func(name, kind, id string) (int, pinsAnswer, string) {
		t.Helper()
		code, data, msg := w.call(t, name, conversationController.GetPins, http.MethodGet, nil, "kind", kind, "id", id)
		var a pinsAnswer
		_ = json.Unmarshal(data, &a)
		return code, a, msg
	}
	pin := func(name, kind, id, message string, pinned bool) (int, pinsAnswer, string) {
		t.Helper()
		code, data, msg := w.call(t, name, conversationController.SetPin, http.MethodPost,
			map[string]any{"message_id": message, "pinned": pinned}, "kind", kind, "id", id)
		var a pinsAnswer
		_ = json.Unmarshal(data, &a)
		return code, a, msg
	}
	pinnedLines := func(channel string) int {
		var got struct {
			C []struct {
				P []struct {
					Text string `json:"post_text"`
				} `json:"ch_posts"`
			}
		}
		q := fmt.Sprintf(`{ c(func: eq(ch_uuid, %q)) { ch_posts @filter(eq(post_kind, "system")) { post_text } } }`, channel)
		_ = json.Unmarshal(w.dg.Query(t, q), &got)
		n := 0
		for _, c := range got.C {
			for _, p := range c.P {
				if strings.Contains(p.Text, "pinned a message") {
					n++
				}
			}
		}
		return n
	}

	t.Run("a member pins a message once, with a quiet line", func(t *testing.T) {
		code, a, msg := pin("maya", "channel", launch, plan, true)
		if code != http.StatusOK || len(a.Pins) != 1 || a.Pins[0].MessageID != plan || a.Pins[0].PinnedBy.Name != "maya" || !a.CanPin || a.Max != 100 {
			t.Fatalf("pinning: %d %+v %q", code, a, msg)
		}
		if code, a, _ := pin("maya", "channel", launch, plan, true); code != http.StatusOK || len(a.Pins) != 1 {
			t.Fatalf("pinning it again: %d %+v", code, a)
		}
		waitFor(t, "the pinned line", func() bool { return pinnedLines(launch) >= 1 })
		if n := pinnedLines(launch); n != 1 {
			t.Fatalf("%d lines say a message was pinned, want one", n)
		}
		// Jonas sees it too, and may pin.
		if code, a, _ := pins("jonas", "channel", launch); code != http.StatusOK || len(a.Pins) != 1 || !a.CanPin || !strings.Contains(a.Pins[0].Message.HTML, plan[:8]) {
			t.Fatalf("Jonas's list: %d %+v", code, a)
		}
	})

	t.Run("who may pin, said in words", func(t *testing.T) {
		// Sam reads the public channel and may not pin in it.
		if code, a, _ := pins("sam", "channel", launch); code != http.StatusOK || len(a.Pins) != 1 || a.CanPin {
			t.Fatalf("Sam's list: %d %+v", code, a)
		}
		if code, _, msg := pin("sam", "channel", launch, recap, true); code != http.StatusForbidden || msg != "Join the channel to pin messages in it." {
			t.Fatalf("Sam pinning: %d %q", code, msg)
		}
		// In an announcement channel, its managers pin.
		if code, _, msg := pin("maya", "channel", news, notice, true); code != http.StatusForbidden || msg != "Only channel managers can pin messages in this channel." {
			t.Fatalf("Maya pinning in #news: %d %q", code, msg)
		}
		if code, a, msg := pin("jonas", "channel", news, notice, true); code != http.StatusOK || len(a.Pins) != 1 {
			t.Fatalf("Jonas pinning in #news: %d %+v %q", code, a, msg)
		}
		// A private channel's pins are nobody else's.
		if code, _, _ := pin("maya", "channel", secret, hush, true); code != http.StatusOK {
			t.Fatalf("Maya pinning in #secret: %d", code)
		}
		if code, _, _ := pins("sam", "channel", secret); code != http.StatusNotFound {
			t.Fatalf("Sam reading #secret's pins: %d, want 404", code)
		}
	})

	t.Run("only a person's live message in this conversation", func(t *testing.T) {
		if code, _, msg := pin("maya", "channel", launch, notice, true); code != http.StatusNotFound || msg != "That message isn't in this conversation." {
			t.Fatalf("#news's message in #launch: %d %q", code, msg)
		}
		if code, _, msg := pin("maya", "channel", launch, line, true); code != http.StatusBadRequest || msg != "A line about the conversation can't be pinned." {
			t.Fatalf("a system line: %d %q", code, msg)
		}
	})

	t.Run("a deleted message's pin goes", func(t *testing.T) {
		if code, a, _ := pin("maya", "channel", launch, recap, true); code != http.StatusOK || len(a.Pins) != 2 {
			t.Fatalf("pinning the recap: %d %+v", code, a)
		}
		var got struct{ P []struct{ UID string } }
		_ = json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ p(func: eq(post_uuid, %q)) { uid } }`, recap)), &got)
		w.dg.Mutate(t, map[string]any{"uid": got.P[0].UID, "post_deleted_at": time.Now().UTC().Format(time.RFC3339)})
		if code, a, _ := pins("maya", "channel", launch); code != http.StatusOK || len(a.Pins) != 1 || a.Pins[0].MessageID != plan {
			t.Fatalf("after the recap was deleted: %d %+v", code, a)
		}
		var n int
		if err := w.env.PG.QueryRow(`SELECT count(*) FROM message_pins WHERE message_id = $1`, recap).Scan(&n); err != nil || n != 0 {
			t.Fatalf("the deleted message's pin row: %d %v", n, err)
		}
	})

	t.Run("a DM's two people pin there", func(t *testing.T) {
		_, data, _ := w.call(t, "jonas", chatController.CreateChat, http.MethodPost, chatAdapter.ChatInfo{ToUuid: w.people["maya"].String(), TextHtml: "<p>The doors open at nine</p>"})
		var created struct {
			UUID string `json:"uuid"`
		}
		_ = json.Unmarshal(data, &created)
		dm := helpers.GetGroupingId(w.people["maya"].String(), w.people["jonas"].String())
		if code, a, msg := pin("maya", "dm", dm, created.UUID, true); code != http.StatusOK || len(a.Pins) != 1 || a.Pins[0].MessageID != created.UUID {
			t.Fatalf("pinning in the DM: %d %+v %q", code, a, msg)
		}
		if code, a, _ := pins("jonas", "dm", dm); code != http.StatusOK || len(a.Pins) != 1 {
			t.Fatalf("Jonas's DM list: %d %+v", code, a)
		}
		if code, _, _ := pins("sam", "dm", dm); code != http.StatusNotFound {
			t.Fatalf("Sam reading their DM's pins: %d, want 404", code)
		}
		if code, a, _ := pin("jonas", "dm", dm, created.UUID, false); code != http.StatusOK || len(a.Pins) != 0 {
			t.Fatalf("unpinning: %d %+v", code, a)
		}
	})

	t.Run("a conversation holds 100", func(t *testing.T) {
		for i := 0; i < 99; i++ {
			if _, err := w.env.PG.Exec(`INSERT INTO message_pins (conversation_kind, conversation_id, message_id, pinned_by) VALUES ('channel', $1, $2, $3)`,
				launch, uuid.New(), w.people["maya"]); err != nil {
				t.Fatal(err)
			}
		}
		code, _, msg := pin("jonas", "channel", launch, recap, true)
		if code == http.StatusOK {
			t.Fatal("a 101st pin was taken")
		}
		code, _, msg = pin("jonas", "channel", launch, uuid.NewString(), true)
		if code != http.StatusNotFound {
			t.Fatalf("a message that isn't there: %d %q", code, msg)
		}
		fresh := uuid.NewString()
		w.channel(t, uuid.NewString(), "spare", false, "", []string{"jonas"}, nil, map[string]string{fresh: "jonas"})
		var gotPost struct{ P []struct{ UID string } }
		_ = json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ p(func: eq(post_uuid, %q)) { uid } }`, fresh)), &gotPost)
		// Move a live message into #launch: the 101st real one.
		var ch struct{ C []struct{ UID string } }
		_ = json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ c(func: eq(ch_uuid, %q)) { uid } }`, launch)), &ch)
		w.dg.Mutate(t, map[string]any{"uid": gotPost.P[0].UID, "post_channel": map[string]any{"uid": ch.C[0].UID}})
		code, _, msg = pin("jonas", "channel", launch, fresh, true)
		if code != http.StatusConflict || msg != "This conversation has 100 pinned messages, the most it holds. Unpin one first." {
			t.Fatalf("a 101st pin: %d %q", code, msg)
		}
	})
}
