//go:build integration
// +build integration

package integration_test

// The conversations index (business/Conversation, chat A9) against real
// Postgres 12, Dgraph and Redis: its migration goes up, down and up again; the
// membership functions keep it beside the graph's edges; the backfill fills it
// in without undoing a removal made beside it; a comparison with the graph
// finds it clean, so access decisions read it, and they decide exactly as the
// graph does; and a difference sends them back to the graph until a comparison
// finds none.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestConversations|TestTheConversationsIndex|TestTheBackfill' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	dmAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	conversationModels "github.com/akashc777/OneCamp/models/postgres/Conversation"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// TestConversationsMigrationUpDownUp: the migration goes up, down and up again
// on Postgres 12, with a number the chat programme's migrations may have, and
// ids made by the application.
func TestConversationsMigrationUpDownUp(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(here), "..", "..", "migrations")
	read := func(direction string) (string, string) {
		matches, _ := filepath.Glob(filepath.Join(dir, "*_conversations."+direction+".sql"))
		if len(matches) != 1 {
			t.Fatalf("want one conversations %s migration, found %v", direction, matches)
		}
		b, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Base(matches[0]), string(b)
	}
	upName, up := read("up")
	_, down := read("down")
	if n, err := strconv.Atoi(strings.SplitN(upName, "_", 2)[0]); err != nil || n == 198 || n == 199 || n < 240 {
		t.Fatalf("%s: a chat programme migration is numbered from 240, and never 198 or 199", upName)
	}
	if strings.Contains(up, "gen_random_uuid") || strings.Contains(up, "uuid_generate") {
		t.Fatalf("%s makes ids in the database; Postgres 12 has neither function by default", upName)
	}
	env := integration.SetupEnv(t)
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {down, false}, {up, true}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		for _, table := range []string{"conversations", "conversation_members", "messages"} {
			if tableExists(t, env.PG, table) != step.want {
				t.Fatalf("step %d: %s exists = %v", i, table, !step.want)
			}
		}
	}
	// A member and a message belong to a conversation that exists, and a
	// message is copied into a conversation once.
	if _, err := env.PG.Exec(`INSERT INTO conversation_members (conversation_id, user_id) VALUES ($1, $2)`, uuid.New(), uuid.New()); err == nil {
		t.Fatal("a member of no conversation was kept")
	}
	conv, original := uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO conversations (id, kind, ref) VALUES ($1, 'group', 'g1')`, conv); err != nil {
		t.Fatal(err)
	}
	copyOf := func() error {
		_, err := env.PG.Exec(`INSERT INTO messages (id, conversation_id, author_id, copied_from, created_at) VALUES ($1, $2, $3, $4, NOW())`,
			uuid.New(), conv, uuid.New(), original)
		return err
	}
	if err := copyOf(); err != nil {
		t.Fatal(err)
	}
	if err := copyOf(); err == nil {
		t.Fatal("a message was copied into one conversation twice")
	}
	if _, err := env.PG.Exec(`INSERT INTO conversations (id, kind, ref) VALUES ($1, 'room', 'r1')`, uuid.New()); err == nil {
		t.Fatal("a conversation of no kind was kept")
	}
}

// indexWorld is the stores the index's tests need, and four people.
type indexWorld struct {
	env    *integration.Env
	dg     *integration.DgraphEnv
	people map[string]uuid.UUID
	uids   map[string]string
}

func newIndexWorld(t *testing.T, names ...string) *indexWorld {
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
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[],"result":"updated","_id":"x"}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	w := &indexWorld{env: env, dg: dg, people: map[string]uuid.UUID{}, uids: map[string]string{}}
	for _, name := range names {
		id := uuid.New()
		w.people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
		got := dg.Mutate(t, map[string]any{"uid": "_:u", "dgraph.type": "User", "user_uuid": id.String(), "user_name": name,
			"user_deleted_at": "0001-01-01T00:00:00Z"})
		w.uids[name] = got["u"]
	}
	return w
}

func (w *indexWorld) info(t *testing.T, name string) *userModels.UserInfo {
	t.Helper()
	ctx := context.Background()
	pg, err := userDomain.GetUserByUUID(ctx, w.people[name])
	if err != nil || pg == nil {
		t.Fatalf("load %s: %v", name, err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(ctx, w.people[name].String())
	if err != nil || gr == nil {
		t.Fatalf("load %s's node: %v", name, err)
	}
	return &userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}
}

// members is the index's people of ref: "name:role" for those in it now,
// "name:out" for those who left, sorted.
func (w *indexWorld) members(t *testing.T, ref string) []string {
	t.Helper()
	byID := map[uuid.UUID]string{}
	for n, id := range w.people {
		byID[id] = n
	}
	rows, err := w.env.PG.Query(`SELECT m.user_id, m.role, m.left_at IS NOT NULL FROM conversation_members m
		JOIN conversations c ON c.id = m.conversation_id WHERE c.ref = $1`, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id uuid.UUID
		var role string
		var out_ bool
		if err := rows.Scan(&id, &role, &out_); err != nil {
			t.Fatal(err)
		}
		if out_ {
			role = "out"
		}
		out = append(out, byID[id]+":"+role)
	}
	sort.Strings(out)
	return out
}

// sameList compares two lists as sets: the order people were pushed in, or
// read in, is the server's to choose.
func sameList(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	got = append([]string(nil), got...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

// The membership functions keep the index beside the graph, a comparison
// finds it clean, decisions read it and decide as the graph does, and a
// difference sends them back to the graph until a comparison finds none.
func TestTheConversationsIndexFollowsTheGraph(t *testing.T) {
	ctx := context.Background()
	w := newIndexWorld(t, "maya", "jonas", "sam", "priya", "olu")
	maya, jonas, sam, priya := w.info(t, "maya"), w.info(t, "jonas"), w.info(t, "sam"), w.info(t, "priya")

	// Maya makes a private channel and lets in Jonas, Sam and Olu.
	err, channel := channelBusiness.CreateChannel(ctx, &channelAdapter.InputCreateChannel{ChannelName: "launch-pricing", ChannelPrivate: true}, maya)
	if err != nil {
		t.Fatalf("making the channel: %v", err)
	}
	ch := channel.String()
	for _, n := range []string{"jonas", "sam", "olu"} {
		if err := channelBusiness.AddChannelMemberEdge(ctx, channel, &w.info(t, n).UserDgraphInfo, w.people[n]); err != nil {
			t.Fatalf("adding %s: %v", n, err)
		}
	}
	sameList(t, "the channel's people", w.members(t, ch), "maya:manager", "jonas:member", "sam:member", "olu:member")

	if err := channelBusiness.AddChannelModeratorEdge(ctx, ch, w.uids["jonas"]); err != nil {
		t.Fatal(err)
	}
	sameList(t, "after Jonas is made a manager", w.members(t, ch), "maya:manager", "jonas:manager", "sam:member", "olu:member")

	chNode := string(w.dg.Query(t, `{ q(func: eq(ch_uuid, "`+ch+`")) { uid } }`))
	var found struct {
		Q []struct {
			UID string `json:"uid"`
		} `json:"q"`
	}
	_ = json.Unmarshal([]byte(chNode), &found)
	if len(found.Q) != 1 {
		t.Fatalf("the channel's node: %s", chNode)
	}
	chUID := found.Q[0].UID
	if err := channelBusiness.DeleteChannelModeratorEdge(ctx, chUID, w.uids["jonas"], ch); err != nil {
		t.Fatal(err)
	}
	if err := channelBusiness.DeleteChannelMemberEdge(ctx, chUID, w.uids["sam"], w.people["sam"].String(), ch); err != nil {
		t.Fatal(err)
	}
	sameList(t, "after Jonas steps down and Sam is taken out", w.members(t, ch), "maya:manager", "jonas:member", "sam:out", "olu:member")

	// Olu is deactivated, in both stores, and stays a member on paper.
	if _, err := w.env.PG.Exec(`UPDATE users SET deleted_at = NOW() WHERE id = $1`, w.people["olu"]); err != nil {
		t.Fatal(err)
	}
	w.dg.Mutate(t, map[string]any{"uid": w.uids["olu"], "user_deleted_at": time.Now().UTC().Format(time.RFC3339)})

	// Maya writes to Priya, and starts a group with Jonas and Priya.
	call := func(who *userModels.UserInfo, handler http.HandlerFunc, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, *who)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		var answer struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		return answer.Data
	}
	dmMessage := call(maya, chatController.CreateChat, dmAdapter.ChatInfo{ToUuid: priya.UserDgraphInfo.Uuid, TextHtml: "<p>Hi Priya</p>"})
	dm := helpers.GetGroupingId(w.people["maya"].String(), w.people["priya"].String())
	sameList(t, "the DM's people", w.members(t, dm), "maya:member", "priya:member")
	groupMessage := call(maya, chatController.CreateGroupChat, dmAdapter.ChatInfo{
		Participants: []string{w.uids["maya"], w.uids["jonas"], w.uids["priya"]}, TextHtml: "<p>Kickoff</p>"})
	group, err := helpers.GenerateGroupID([]string{w.people["maya"].String(), w.people["jonas"].String(), w.people["priya"].String()})
	if err != nil {
		t.Fatal(err)
	}
	sameList(t, "the group's people", w.members(t, group), "maya:member", "jonas:member", "priya:member")

	// Until the index has been filled in from the graph, decisions read the
	// graph; the job fills it in (the membership functions wrote it all
	// already) and marks it done.
	if c := conversationDomain.AccessSource(ctx); c.Index || !strings.Contains(c.Why, "filled in") {
		t.Fatalf("an index not filled in yet is read: %+v", c)
	}
	if rep, err := conversationBusiness.Backfill(ctx); err != nil || rep.Members != 0 {
		t.Fatalf("the backfill after the membership functions wrote everything: %s (%v)", rep, err)
	}
	if err := configModel.UpsertConfig(conversationModels.BackfillKey, "done: filled in by the test"); err != nil {
		t.Fatal(err)
	}

	// A comparison with the graph a tick later, past its margin, checks every
	// row and finds nothing to repair, and decisions read the index.
	pastTheMargin(t, w.env)
	from, _ := conversationDomain.StateNow(ctx)
	rep, err := conversationBusiness.Reconcile(ctx, from)
	if err != nil || rep.Repaired != 0 || !rep.Settled {
		t.Fatalf("the first comparison: %s (%v)", rep, err)
	}
	if c := conversationDomain.AccessSource(ctx); !c.Index {
		t.Fatalf("a clean index isn't read: %s", c.Why)
	}

	// Every person's decision on every conversation, from wherever decisions
	// read now.
	refs := []conversationBusiness.Ref{
		{Kind: conversationBusiness.KindChannel, ID: ch},
		{Kind: conversationBusiness.KindDM, ID: dm},
		{Kind: conversationBusiness.KindGroup, ID: group},
		{Kind: conversationBusiness.KindThread, ID: fmt.Sprint(dmMessage["uuid"])},
		{Kind: conversationBusiness.KindThread, ID: fmt.Sprint(groupMessage["uuid"])},
		{Kind: conversationBusiness.KindCall, ID: ch},
		{Kind: conversationBusiness.KindCall, ID: group},
	}
	everyone := map[string]*userModels.UserInfo{"maya": maya, "jonas": jonas, "sam": sam, "priya": priya}
	decisions := func() map[string]string {
		out := map[string]string{}
		for name, who := range everyone {
			for _, ref := range refs {
				d, err := conversationBusiness.Resolve(ctx, who, ref)
				out[name+" "+ref.Kind+" "+ref.ID] = fmt.Sprintf("%s %v %v", d.Level, d.Archived, err)
			}
		}
		return out
	}
	fromIndex := decisions()
	conversationDomain.MarkDirty(ctx, "the test asks the graph")
	if c := conversationDomain.AccessSource(ctx); c.Index {
		t.Fatal("a dirty index is read")
	}
	fromGraph := decisions()
	for k, v := range fromGraph {
		if fromIndex[k] != v {
			t.Errorf("%s: the graph decides %q, the index %q", k, v, fromIndex[k])
		}
	}
	if got := fromIndex["sam channel "+ch]; !strings.Contains(got, "conversation: no such conversation") {
		t.Errorf("Sam, taken out of a private channel, decides %q", got)
	}
	if got := fromIndex["jonas channel "+ch]; !strings.HasPrefix(got, "edit") {
		t.Errorf("Jonas, a member, decides %q", got)
	}

	// A comparison makes the index clean again; then a row the index lost
	// sends decisions back to the graph, which a comparison repairs.
	from, _ = conversationDomain.StateNow(ctx)
	if rep, err = conversationBusiness.Reconcile(ctx, from); err != nil || rep.Repaired != 0 {
		t.Fatalf("the second comparison: %s (%v)", rep, err)
	}
	if _, err := w.env.PG.Exec(`DELETE FROM conversation_members m USING conversations c
		WHERE c.id = m.conversation_id AND c.ref = $1 AND m.user_id = $2`, ch, w.people["jonas"]); err != nil {
		t.Fatal(err)
	}
	// Older than the comparison's margin, as a row lost long ago would be.
	if _, err := w.env.PG.Exec(`UPDATE conversation_members SET updated_at = NOW() - INTERVAL '1 hour'`); err != nil {
		t.Fatal(err)
	}
	from, _ = conversationDomain.StateNow(ctx)
	if rep, err = conversationBusiness.Reconcile(ctx, from); err != nil || rep.Repaired != 1 {
		t.Fatalf("the comparison after a lost row: %s (%v)", rep, err)
	}
	if c := conversationDomain.AccessSource(ctx); c.Index || !strings.Contains(c.Why, "repaired") {
		t.Fatalf("decisions still read an index that had to be repaired: %+v", c)
	}
	sameList(t, "the repaired channel", w.members(t, ch), "maya:manager", "jonas:member", "sam:out", "olu:member")
	// The next comparison is a tick later, past the margin of the row the
	// repair wrote.
	pastTheMargin(t, w.env)
	from, _ = conversationDomain.StateNow(ctx)
	if rep, err = conversationBusiness.Reconcile(ctx, from); err != nil || rep.Repaired != 0 {
		t.Fatalf("the comparison after the repair: %s (%v)", rep, err)
	}
	if c := conversationDomain.AccessSource(ctx); !c.Index {
		t.Fatalf("a comparison that found nothing didn't bring decisions back to the index: %s", c.Why)
	}
}

// The backfill writes what the graph has, and leaves alone a removal written
// beside it.
func TestTheBackfillFillsInTheIndexWithoutUndoingARemoval(t *testing.T) {
	ctx := context.Background()
	w := newIndexWorld(t, "ada", "bo", "cy")
	channel := uuid.New()
	if _, err := w.env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'general', false)`, channel); err != nil {
		t.Fatal(err)
	}
	group, err := helpers.GenerateGroupID([]string{w.people["ada"].String(), w.people["bo"].String(), w.people["cy"].String()})
	if err != nil {
		t.Fatal(err)
	}
	dm := helpers.GetGroupingId(w.people["ada"].String(), w.people["bo"].String())
	w.dg.Mutate(t, []map[string]any{
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(), "ch_name": "general", "ch_private": false,
			"ch_deleted_at": "0001-01-01T00:00:00Z", "ch_created_by": map[string]any{"uid": w.uids["ada"]},
			"ch_members":    []map[string]any{{"uid": w.uids["ada"]}, {"uid": w.uids["bo"]}, {"uid": w.uids["cy"]}},
			"ch_moderators": []map[string]any{{"uid": w.uids["ada"]}}},
		{"uid": "_:g", "dgraph.type": "Dm", "dm_grouping_id": group,
			"dm_participants": []map[string]any{{"uid": w.uids["ada"]}, {"uid": w.uids["bo"]}, {"uid": w.uids["cy"]}}},
		{"uid": "_:d", "dgraph.type": "Dm", "dm_grouping_id": dm,
			"dm_participants": []map[string]any{{"uid": w.uids["ada"]}, {"uid": w.uids["bo"]}}},
	})
	// Cy was taken out of the channel after the backfill read it.
	id, err := conversationModels.Ensure(ctx, conversationModels.Conversation{ID: channel, Kind: conversationModels.KindChannel, Ref: channel.String()})
	if err != nil {
		t.Fatal(err)
	}
	if err := conversationModels.Leave(ctx, id, []uuid.UUID{w.people["cy"]}, time.Now()); err != nil {
		t.Fatal(err)
	}

	rep, err := conversationBusiness.Backfill(ctx)
	if err != nil || rep.Conversations != 3 {
		t.Fatalf("the backfill: %s (%v)", rep, err)
	}
	sameList(t, "the channel", w.members(t, channel.String()), "ada:manager", "bo:member", "cy:out")
	sameList(t, "the group", w.members(t, group), "ada:member", "bo:member", "cy:member")
	sameList(t, "the DM", w.members(t, dm), "ada:member", "bo:member")
	var kinds string
	if err := w.env.PG.QueryRow(`SELECT string_agg(kind, ',' ORDER BY kind) FROM conversations`).Scan(&kinds); err != nil || kinds != "channel,dm,group" {
		t.Fatalf("the conversations' kinds: %q (%v)", kinds, err)
	}
	// Run again, it writes nothing more.
	if again, err := conversationBusiness.Backfill(ctx); err != nil || again.Members != 0 {
		t.Fatalf("a second backfill: %s (%v)", again, err)
	}
}

// pastTheMargin makes every row of the index older than a comparison's margin,
// as the job's next tick, ten minutes on, finds them.
func pastTheMargin(t *testing.T, env *integration.Env) {
	t.Helper()
	if _, err := env.PG.Exec(`UPDATE conversation_members SET updated_at = NOW() - INTERVAL '10 minutes'`); err != nil {
		t.Fatal(err)
	}
}

// The demo's first boot (17 Oct 2026): the backfill filled the index in, and a
// comparison at once left every row to the next one, written inside its margin,
// yet called the index clean, so the next real comparison was a day away. A
// comparison that left rows it was due to check leaves the index unchecked and
// decisions on the graph; the next one, past the margin, checks the rows, and
// then decisions read the index.
func TestABackfilledIndexIsReadOnlyOnceAComparisonHasCheckedIt(t *testing.T) {
	ctx := context.Background()
	w := newIndexWorld(t, "ada", "bo")
	channel := uuid.New()
	if _, err := w.env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'board', true)`, channel); err != nil {
		t.Fatal(err)
	}
	w.dg.Mutate(t, map[string]any{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel.String(), "ch_name": "board", "ch_private": true,
		"ch_deleted_at": "0001-01-01T00:00:00Z", "ch_created_by": map[string]any{"uid": w.uids["ada"]},
		"ch_members":    []map[string]any{{"uid": w.uids["ada"]}, {"uid": w.uids["bo"]}},
		"ch_moderators": []map[string]any{{"uid": w.uids["ada"]}}})
	if rep, err := conversationBusiness.Backfill(ctx); err != nil || rep.Members != 2 {
		t.Fatalf("the backfill: %s (%v)", rep, err)
	}
	if err := configModel.UpsertConfig(conversationModels.BackfillKey, "done: filled in by the test"); err != nil {
		t.Fatal(err)
	}
	// Whatever an earlier test's decision left kept, the next one reads the
	// state.
	t.Cleanup(conversationDomain.StartedForTest(conversationDomain.StartedAt()))

	from, _ := conversationDomain.StateNow(ctx)
	rep, err := conversationBusiness.Reconcile(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := conversationDomain.StateNow(ctx); v != "" || rep.Settled || rep.Deferred != 2 {
		t.Fatalf("a comparison at once after the backfill, which checked no row, left the state %q (%s)", v, rep)
	}
	if c := conversationDomain.AccessSource(ctx); c.Index {
		t.Fatalf("decisions read an index no comparison has checked: %s", c.Why)
	}

	pastTheMargin(t, w.env)
	from, _ = conversationDomain.StateNow(ctx)
	if rep, err = conversationBusiness.Reconcile(ctx, from); err != nil || rep.Repaired != 0 || rep.Deferred != 0 || !rep.Settled {
		t.Fatalf("the comparison a tick later: %s (%v)", rep, err)
	}
	if c := conversationDomain.AccessSource(ctx); !c.Index {
		t.Fatalf("a comparison that checked every row didn't bring decisions to the index: %s", c.Why)
	}
}
