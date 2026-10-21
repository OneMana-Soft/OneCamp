//go:build integration
// +build integration

package integration_test

// Mentions that reach (chat P3), against real Postgres 12, Redis and Dgraph,
// through the handlers the app calls. @channel tells everyone in the channel
// but its writer and its agents, by their setting for it; @here only those
// here now (a device connected, not set offline); a team's mention its people
// in the channel and nobody outside it. The rules: members may by default, a
// channel's managers can say not for it and the workspace for all; above a
// size the workspace sets only managers may; through a token nobody acts as a
// manager. A refusal is in words and sends nothing. A broadcast in a thread
// reaches nobody, and an edit keeps whom its post reached.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestMentionsThatReach' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// reachWorld is #launch: Maya writes; Jonas manages it and is here; Sam has
// the app open but set himself offline; Ada is away; Scout is an agent in it;
// Zed isn't in it. The team "design" is Ada, Zed and Maya.
type reachWorld struct {
	env     *integration.Env
	dg      *integration.DgraphEnv
	channel uuid.UUID
	team    uuid.UUID
	people  map[string]uuid.UUID
	uids    map[string]string
	names   map[string]string // user uuid → name

	mu     sync.Mutex
	pushes []sentPush
}

func newReachWorld(t *testing.T) *reachWorld {
	t.Helper()
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
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

	w := &reachWorld{env: env, dg: dg, channel: uuid.New(), team: uuid.New(),
		people: map[string]uuid.UUID{}, uids: map[string]string{}, names: map[string]string{}}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, 'launch', false)`, w.channel)
	// Each person's level for #launch: Maya hears of everything, the rest of
	// mentions only, so a push to them is a mention's.
	levels := map[string]string{"maya": "all", "jonas": "mention", "sam": "mention", "ada": "mention", "scout": "mention"}
	var nodes []map[string]any
	for _, name := range []string{"maya", "jonas", "sam", "ada", "scout", "zed"} {
		id := uuid.New()
		w.people[name], w.names[id.String()] = id, name
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name)
		if name != "scout" {
			exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-' || $2)`, id, name)
		}
		if level, in := levels[name]; in {
			exec(`INSERT INTO users_channel_notification (user_id, channel_id, notification_type) VALUES ($1, $2, $3)`, id, w.channel, level)
		}
		node := map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": id.String(), "user_name": name,
			"user_deleted_at": liveAt}
		switch name {
		case "jonas":
			node["user_device_connected"], node["user_status"] = 1, "online"
		case "sam":
			node["user_device_connected"], node["user_status"] = 1, "offline"
		case "scout":
			node["user_device_connected"], node["is_bot"] = 1, true
		}
		nodes = append(nodes, node)
	}
	ref := func(names ...string) []map[string]any {
		var out []map[string]any
		for _, n := range names {
			out = append(out, map[string]any{"uid": "_:" + n})
		}
		return out
	}
	got := dg.Mutate(t, append(nodes,
		map[string]any{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": w.channel.String(), "ch_name": "launch",
			"ch_private": false, "ch_deleted_at": liveAt, "ch_members": ref("maya", "jonas", "sam", "ada", "scout"),
			"ch_moderators": ref("jonas")},
		map[string]any{"uid": "_:team", "dgraph.type": "Team", "team_uuid": w.team.String(), "team_name": "design",
			"team_deleted_at": liveAt, "team_members": ref("ada", "zed", "maya")},
	))
	for name := range w.people {
		w.uids[name] = got[name]
	}
	t.Cleanup(firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		w.mu.Lock()
		w.pushes = append(w.pushes, sentPush{data, tokens})
		w.mu.Unlock()
	}))
	return w
}

// as is a person as the routes see them; through a token when viaToken.
func (w *reachWorld) as(t *testing.T, name string, viaToken bool) context.Context {
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
	if viaToken {
		ctx = helpers.WithActsThroughToken(ctx)
	}
	return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
}

// run runs handler with body in ctx, answering its status, data and words.
func (w *reachWorld) run(ctx context.Context, handler http.HandlerFunc, method, query string, body any) (int, json.RawMessage, string) {
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(method, "/"+query, bytes.NewReader(raw)).WithContext(ctx))
	var out struct {
		Data json.RawMessage `json:"data"`
		Msg  string          `json:"msg"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.Data, out.Msg
}

// post sends html in #launch as ctx's person, answering the status, the
// post's id and the words.
func (w *reachWorld) post(t *testing.T, ctx context.Context, html string) (int, string, string) {
	t.Helper()
	code, data, msg := w.run(ctx, postController.CreatePost, http.MethodPost, "", postAdapter.InputCreateOrUpdatePostInfo{ChannelUuid: w.channel.String(), HTMLText: html})
	var created struct {
		Uuid string `json:"uuid"`
	}
	_ = json.Unmarshal(data, &created)
	return code, created.Uuid, msg
}

// mentioned is who the graph holds as told by the message idPred names, by
// name, sorted: the edges every Inbox, badge and notice reads.
func (w *reachWorld) mentioned(t *testing.T, idPred, edge, id string) []string {
	t.Helper()
	var got struct {
		M []struct {
			E []struct {
				Users []struct {
					UUID string `json:"user_uuid"`
				} `json:"mention_users"`
			} `json:"e"`
		} `json:"m"`
	}
	q := fmt.Sprintf(`{ m(func: eq(%s, %q)) { e: %s { mention_users { user_uuid } } } }`, idPred, id, edge)
	if err := json.Unmarshal(w.dg.Query(t, q), &got); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range got.M {
		for _, e := range m.E {
			for _, u := range e.Users {
				names = append(names, w.names[u.UUID])
			}
		}
	}
	sort.Strings(names)
	return names
}

// pushedFor is who was pushed of the message (by name, sorted), waiting for
// want's pushes to land first, then a moment for any that shouldn't.
func (w *reachWorld) pushedFor(t *testing.T, message string, want []string) []string {
	t.Helper()
	read := func() []string {
		w.mu.Lock()
		defer w.mu.Unlock()
		var names []string
		for _, p := range w.pushes {
			if p.data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] != message {
				continue
			}
			for _, tok := range p.tokens {
				if n := tok[len("tok-"):]; !slices.Contains(names, n) {
					names = append(names, n)
				}
			}
		}
		sort.Strings(names)
		return names
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got := read()
		if len(want) == 0 || !slices.ContainsFunc(want, func(n string) bool { return !slices.Contains(got, n) }) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	return read()
}

func chip(id, label string) string {
	return fmt.Sprintf(`<span data-type="mention" data-id="%s">@%s</span>`, id, label)
}

func TestMentionsThatReach(t *testing.T) {
	w := newReachWorld(t)
	maya, jonas := w.as(t, "maya", false), w.as(t, "jonas", false)
	everyone, here, design := chip("broadcast:channel", "channel"), chip("broadcast:here", "here"), chip("group:"+w.team.String(), "design")

	// @channel: everyone in #launch but Maya and the agent, by their setting.
	code, all, msg := w.post(t, maya, "<p>"+everyone+" the launch moved to 10</p>")
	if code != http.StatusOK || all == "" {
		t.Fatalf("@channel: %d %s", code, msg)
	}
	if got := w.mentioned(t, "post_uuid", "post_mentions", all); !slices.Equal(got, []string{"ada", "jonas", "sam"}) {
		t.Errorf("@channel told %v: Ada, Jonas and Sam, never Maya or Scout", got)
	}
	if got := w.pushedFor(t, all, []string{"ada", "jonas", "sam"}); !slices.Equal(got, []string{"ada", "jonas", "sam"}) {
		t.Errorf("@channel pushed %v", got)
	}

	// @here: only who is here now. Sam set himself offline; Ada is away.
	_, now, _ := w.post(t, maya, "<p>"+here+" who's around?</p>")
	if got := w.mentioned(t, "post_uuid", "post_mentions", now); !slices.Equal(got, []string{"jonas"}) {
		t.Errorf("@here told %v: Jonas alone", got)
	}
	if got := w.pushedFor(t, now, []string{"jonas"}); !slices.Equal(got, []string{"jonas"}) {
		t.Errorf("@here pushed %v", got)
	}

	// A team's mention: its people in the channel, never Zed, who isn't.
	_, team, _ := w.post(t, maya, "<p>"+design+" mock-ups?</p>")
	if got := w.mentioned(t, "post_uuid", "post_mentions", team); !slices.Equal(got, []string{"ada"}) {
		t.Errorf("@design told %v: Ada, never Zed", got)
	}
	if got := w.pushedFor(t, team, []string{"ada"}); !slices.Equal(got, []string{"ada"}) {
		t.Errorf("@design pushed %v", got)
	}

	// In a thread a broadcast reaches nobody; a team's mention still does.
	code, data, msg := w.run(maya, postController.CreateCommentInPost, http.MethodPost, "", postAdapter.InputCreateOrUpdateCommentToPost{
		PostUuid: all, HTMLText: "<p>" + everyone + design + " in the thread</p>"})
	var reply struct {
		Uuid string `json:"comment_id"`
	}
	if code != http.StatusOK || json.Unmarshal(data, &reply) != nil || reply.Uuid == "" {
		t.Fatalf("a thread reply: %d %s %s", code, data, msg)
	}
	if got := w.mentioned(t, "comment_uuid", "comment_mentions", reply.Uuid); !slices.Equal(got, []string{"ada"}) {
		t.Errorf("a thread reply's @channel and @design told %v: Ada alone", got)
	}

	// The channel says members may not: Maya is refused in words, and nothing
	// is sent; Jonas, its manager, may; through a token he is a member.
	ctx := context.Background()
	by := policyBusiness.Changer{UserID: w.people["jonas"], Email: "jonas@acme.test"}
	launchScope := policyBusiness.Scope{Kind: "channel", ID: w.channel.String()}
	if _, err := policyBusiness.Admin.SetIn(ctx, "chat.broadcast.members", launchScope, json.RawMessage("false"), by); err != nil {
		t.Fatal(err)
	}
	if code, id, msg := w.post(t, maya, "<p>"+everyone+" again</p>"); code != http.StatusForbidden || id != "" || msg != "Only #launch's managers can notify everyone in it." {
		t.Errorf("Maya's @channel where members may not: %d %q %q", code, id, msg)
	}
	code, managed, msg := w.post(t, jonas, "<p>"+here+" standup</p>")
	if code != http.StatusOK {
		t.Fatalf("Jonas's @here as the channel's manager: %d %s", code, msg)
	}
	if got := w.mentioned(t, "post_uuid", "post_mentions", managed); len(got) != 0 {
		t.Errorf("Jonas's @here told %v: nobody else is here", got)
	}
	if code, _, msg := w.post(t, w.as(t, "jonas", true), "<p>"+everyone+"</p>"); code != http.StatusForbidden {
		t.Errorf("Jonas's @channel through a token: %d %q", code, msg)
	}

	// An edit keeps whom its post reached, and isn't refused for a broadcast
	// it was sent with; one it adds is asked of.
	edit := func(post, html string) (int, string) {
		code, _, msg := w.run(maya, postController.UpdatePost, http.MethodPut, "", postAdapter.InputCreateOrUpdatePostInfo{Uuid: post, HTMLText: html})
		return code, msg
	}
	if code, msg := edit(all, "<p>"+everyone+" the launch moved to 10:30</p>"); code != http.StatusOK {
		t.Fatalf("editing the @channel post: %d %s", code, msg)
	}
	if got := w.mentioned(t, "post_uuid", "post_mentions", all); !slices.Equal(got, []string{"ada", "jonas", "sam"}) {
		t.Errorf("after an edit the @channel post tells %v", got)
	}
	if code, msg := edit(team, "<p>"+design+here+" mock-ups?</p>"); code != http.StatusForbidden {
		t.Errorf("an edit adding @here where members may not: %d %s", code, msg)
	}

	// Back to the workspace's (members may), and a size above which only
	// managers may: #launch's five are more than three.
	if _, err := policyBusiness.Admin.ClearIn(ctx, "chat.broadcast.members", launchScope, by); err != nil {
		t.Fatal(err)
	}
	if _, err := policyBusiness.Admin.Set(ctx, "chat.broadcast.admins_only_at", json.RawMessage("3"), by); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = policyBusiness.Admin.Set(context.Background(), "chat.broadcast.admins_only_at", json.RawMessage("0"), by)
	})
	if code, _, msg := w.post(t, maya, "<p>"+here+"</p>"); code != http.StatusForbidden || msg != "#launch has more than 3 members, so only its managers can notify all of it." {
		t.Errorf("Maya's @here past the size: %d %q", code, msg)
	}
	if code, _, msg := w.post(t, maya, "<p>"+design+" still reaches</p>"); code != http.StatusOK {
		t.Errorf("a team's mention past the size: %d %q", code, msg)
	}
}
