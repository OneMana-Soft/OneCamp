//go:build integration
// +build integration

package integration_test

// Who may open a conversation, and what each refusal answers, against real
// Postgres 12 and Dgraph (H5B). A group's pages and a message's thread ask
// whether the reader is in the conversation in the page's own graph read; a
// DM's pages ask whether the other person exists only when the page comes
// back empty. Each answer is the one the routes gave before the gate moved,
// but two: a group that isn't there on the older page dereferenced nil (500,
// V-1190), and a DM with someone who isn't there answered 200 and marked it
// read (the user read gives no row as a zero user, so its nil check never
// fired). Both are the 400 their siblings give:
//
//	                         group page   DM page   thread
//	member / own DM          200          200       200
//	non-member               403          -         403
//	removed member           403          -         403
//	unknown group / message  400          -         404
//	deleted message          -            -         404
//	unknown counterpart      -            400       -
//	deactivated counterpart  -            200       -
//	empty DM                 -            200       -
//
// A refused open marks nothing read. Each page's rows and order are pinned
// too: the four directions read through one page function.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestOpeningA' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// openWorld is a group of ada, bea and cal (cal since removed, after
// writing), DMs of ada's with bea, a deactivated dex and a silent fay, and
// eve, in none of them.
type openWorld struct {
	t      *testing.T
	ctx    context.Context
	env    *integration.Env
	people map[string]uuid.UUID
	uids   map[string]string
	group  string
	t0     time.Time
	// groupChats are the group's live messages, oldest first (t0 + i s);
	// gone is a deleted one with no replies; dmChats are ada's with bea.
	groupChats, dmChats, dexChats []string
	gone                          string
}

func newOpenWorld(t *testing.T) *openWorld {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	t.Cleanup(func() { helpers.SettleBackground(10 * time.Second) })

	w := &openWorld{t: t, ctx: ctx, env: env, people: map[string]uuid.UUID{}, uids: map[string]string{},
		group: strings.ReplaceAll(uuid.NewString(), "-", ""), t0: time.Now().Add(-time.Hour).Truncate(time.Second)}
	for _, name := range []string{"ada", "bea", "cal", "eve", "dex", "fay"} {
		id := uuid.New()
		w.people[name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
		w.uids[name] = dg.Mutate(t, map[string]any{"uid": "_:u", "dgraph.type": "User", "user_uuid": id.String(), "user_name": name})["u"]
	}
	if _, err := env.PG.Exec(`UPDATE users SET deleted_at = NOW() WHERE id = $1`, w.people["dex"]); err != nil {
		t.Fatal(err)
	}
	person := func(name string) map[string]any { return map[string]any{"uid": w.uids[name]} }
	chat := func(at time.Time, by string, deleted time.Time) (string, map[string]any) {
		id := uuid.NewString()
		return id, map[string]any{"dgraph.type": "Chat", "chat_uuid": id, "chat_body_text": "<p>" + by + "</p>",
			"chat_created_at": at.Format(time.RFC3339Nano), "chat_deleted_at": deleted.Format(time.RFC3339Nano),
			"chat_from": person(by), "chat_dm": map[string]any{"uid": "_:dm"}}
	}
	live := time.Unix(0, 0).UTC()
	conversation := func(grouping string, members []string, chats []map[string]any) {
		var ps []map[string]any
		for _, m := range members {
			ps = append(ps, person(m))
		}
		dg.Mutate(t, map[string]any{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": grouping,
			"dm_participants": ps, "dm_chats": chats})
	}
	var nodes []map[string]any
	for i := range 13 {
		id, n := chat(w.t0.Add(time.Duration(i)*time.Second), []string{"ada", "bea", "cal"}[i%3], live)
		w.groupChats = append(w.groupChats, id)
		nodes = append(nodes, n)
	}
	gone, n := chat(w.t0.Add(-10*time.Second), "ada", w.t0)
	w.gone = gone
	conversation(w.group, []string{"ada", "bea", "cal"}, append(nodes, n))

	nodes = nil
	for i := range 3 {
		id, n := chat(w.t0.Add(time.Duration(i)*time.Second), []string{"bea", "ada"}[i%2], live)
		w.dmChats = append(w.dmChats, id)
		nodes = append(nodes, n)
	}
	conversation(w.dm("bea"), []string{"ada", "bea"}, nodes)
	nodes = nil
	for i := range 2 {
		id, n := chat(w.t0.Add(time.Duration(i)*time.Second), "dex", live)
		w.dexChats = append(w.dexChats, id)
		nodes = append(nodes, n)
	}
	conversation(w.dm("dex"), []string{"ada", "dex"}, nodes)

	// Cal leaves the way the app takes someone out: both edges go.
	if err := chatDomain.LeaveGroup(ctx, w.group, w.uids["cal"]); err != nil {
		t.Fatalf("cal leaves the group: %v", err)
	}
	return w
}

// dm is ada's DM grouping with name.
func (w *openWorld) dm(name string) string {
	return helpers.GetGroupingId(w.people["ada"].String(), w.people[name].String())
}

// as is ctx with name signed in, as the auth middleware leaves it.
func (w *openWorld) as(name string) context.Context {
	w.t.Helper()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(w.ctx, w.people[name])
	if err != nil || pg == nil {
		w.t.Fatalf("load %s: %v", name, err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(w.ctx, w.people[name].String())
	if err != nil || gr == nil {
		w.t.Fatalf("load %s's node: %v", name, err)
	}
	return context.WithValue(w.ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
}

// openPage is a page's answer: its messages' uuids in the order sent.
type openPage struct {
	Chats   []string
	HasMore bool
}

// get answers a GET of handler as name with the URL params given (name,
// value, name, value, ...): its status and, on a 200, its page. A handler
// that panics answers 500, as the router's Recoverer makes it.
func (w *openWorld) get(name string, handler http.HandlerFunc, params ...string) (code int, p openPage) {
	w.t.Helper()
	rc := chi.NewRouteContext()
	for i := 0; i+1 < len(params); i += 2 {
		rc.URLParams.Add(params[i], params[i+1])
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(w.as(name), chi.RouteCtxKey, rc))
	rec := httptest.NewRecorder()
	defer func() {
		if recover() != nil {
			code, p = http.StatusInternalServerError, openPage{}
		}
	}()
	handler(rec, req)
	var out struct {
		Data struct {
			Chats []struct {
				UUID string `json:"chat_uuid"`
			} `json:"chats"`
			HasMore bool `json:"has_more"`
		} `json:"data"`
	}
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			w.t.Fatalf("a page that isn't one: %v: %.200s", err, rec.Body.String())
		}
		for _, c := range out.Data.Chats {
			p.Chats = append(p.Chats, c.UUID)
		}
		p.HasMore = out.Data.HasMore
	}
	return rec.Code, p
}

// marked is whether who has a read marker in grouping.
func (w *openWorld) marked(who, grouping string) bool {
	w.t.Helper()
	var n int
	if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM last_seen_chat WHERE user_id = $1 AND grp_id = $2`, w.people[who], grouping).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n > 0
}

// seconds is t0 + s as a page edge's time_stamp.
func (w *openWorld) seconds(s int) string {
	return strconv.FormatInt(w.t0.Add(time.Duration(s)*time.Second).Unix(), 10)
}

// rev is ids newest first.
func rev(ids []string) []string {
	out := slices.Clone(ids)
	slices.Reverse(out)
	return out
}

func samePage(t *testing.T, what string, got openPage, want []string, more bool) {
	t.Helper()
	if !slices.Equal(got.Chats, want) || got.HasMore != more {
		t.Errorf("%s: %d messages %v, has_more=%v; want %d %v, has_more=%v", what, len(got.Chats), got.Chats, got.HasMore, len(want), want, more)
	}
}

// Every group page asks the gate in its own read: a member reads it, in its
// order; a non-member and a removed member (with their own messages on the
// page) are refused 403; a group that isn't there is a 400. A refused latest
// page marks nothing read.
func TestOpeningAGroupPageAnswersAsItsGateDoes(t *testing.T) {
	w := newOpenWorld(t)
	g := w.groupChats
	pages := []struct {
		name    string
		handler http.HandlerFunc
		params  func(grp string) []string
		want    []string
		more    bool
	}{
		{"latest", chatController.GetLatestGroupChats, func(grp string) []string { return []string{"grp_id", grp} }, rev(g[3:]), true},
		{"older", chatController.GetOldGroupChats, func(grp string) []string { return []string{"grp_id", grp, "time_stamp", w.seconds(5)} }, rev(g[:5]), false},
		{"newer", chatController.GetNewGroupChats, func(grp string) []string { return []string{"grp_id", grp, "time_stamp", w.seconds(5)} }, g[6:], false},
		{"newer from a message", chatController.GetNewGroupChatsIncludingChat, func(grp string) []string { return []string{"grp_id", grp, "chat_uuid", g[2]} }, g[2:12], true},
	}
	for _, p := range pages {
		t.Run(p.name, func(t *testing.T) {
			code, page := w.get("ada", p.handler, p.params(w.group)...)
			if code != http.StatusOK {
				t.Fatalf("a member: %d, want 200", code)
			}
			samePage(t, "a member's page", page, p.want, p.more)
			for _, who := range []string{"eve", "cal"} {
				if code, page := w.get(who, p.handler, p.params(w.group)...); code != http.StatusForbidden {
					t.Errorf("%s, not in the group: %d %v, want 403", who, code, page.Chats)
				}
			}
			if code, _ := w.get("ada", p.handler, p.params(strings.ReplaceAll(uuid.NewString(), "-", ""))...); code != http.StatusBadRequest {
				t.Errorf("a group that isn't there: %d, want 400", code)
			}
		})
	}
	// The newer-from page looks its message up before the gate; when that
	// lookup fails it asks the gate again, so a non-member is still refused
	// 403 and can't learn from a 400 whether a message uuid exists anywhere.
	t.Run("newer from a message that isn't there", func(t *testing.T) {
		from := chatController.GetNewGroupChatsIncludingChat
		missing := uuid.NewString()
		for _, who := range []string{"eve", "cal"} {
			if code, page := w.get(who, from, "grp_id", w.group, "chat_uuid", missing); code != http.StatusForbidden {
				t.Errorf("%s, not in the group, from a message that isn't there: %d %v, want 403", who, code, page.Chats)
			}
		}
		if code, _ := w.get("ada", from, "grp_id", w.group, "chat_uuid", missing); code != http.StatusBadRequest {
			t.Errorf("a member, from a message that isn't there: %d, want 400", code)
		}
		if code, _ := w.get("ada", from, "grp_id", strings.ReplaceAll(uuid.NewString(), "-", ""), "chat_uuid", missing); code != http.StatusBadRequest {
			t.Errorf("a group and a message that aren't there: %d, want 400", code)
		}
	})
	if !w.marked("ada", w.group) {
		t.Error("ada opened the group and nothing marked it read")
	}
	for _, who := range []string{"eve", "cal"} {
		if w.marked(who, w.group) {
			t.Errorf("%s was refused the group and it was marked read for them", who)
		}
	}
}

// A DM's pages: with someone who has written, read in order (a deactivated
// person's too); with someone known who never wrote, an empty page; with
// someone who isn't there, a 400 that marks nothing read.
func TestOpeningADMPageAnswersAsItsCounterpartDoes(t *testing.T) {
	w := newOpenWorld(t)
	d := w.dmChats
	latest := func(other string) []string { return []string{"user_uuid", other} }
	older := func(other string) []string { return []string{"user_uuid", other, "time_stamp", w.seconds(2)} }
	newer := func(other string) []string { return []string{"user_uuid", other, "time_stamp", w.seconds(0)} }
	pages := []struct {
		name    string
		handler http.HandlerFunc
		params  func(other string) []string
		want    []string
	}{
		{"latest", chatController.GetLatestChats, latest, rev(d)},
		{"older", chatController.GetOldChats, older, rev(d[:2])},
		{"newer", chatController.GetNewChats, newer, d[1:]},
	}
	ghost := uuid.New()
	for _, p := range pages {
		t.Run(p.name, func(t *testing.T) {
			code, page := w.get("ada", p.handler, p.params(w.people["bea"].String())...)
			if code != http.StatusOK {
				t.Fatalf("ada with bea: %d, want 200", code)
			}
			samePage(t, "ada with bea", page, p.want, false)
			if code, page := w.get("ada", p.handler, p.params(w.people["dex"].String())...); code != http.StatusOK || len(page.Chats) == 0 {
				t.Errorf("ada with dex, deactivated: %d with %d messages, want 200 with his", code, len(page.Chats))
			}
			if code, page := w.get("ada", p.handler, p.params(w.people["fay"].String())...); code != http.StatusOK || len(page.Chats) != 0 || page.HasMore {
				t.Errorf("ada with fay, who never wrote: %d %v, want 200 and an empty page", code, page)
			}
			if code, _ := w.get("ada", p.handler, p.params(ghost.String())...); code != http.StatusBadRequest {
				t.Errorf("ada with someone who isn't there: %d, want 400", code)
			}
		})
	}
	if code, page := w.get("ada", chatController.GetLatestChats, latest(w.people["dex"].String())...); code != http.StatusOK || !slices.Equal(page.Chats, rev(w.dexChats)) {
		t.Errorf("ada with dex: %d %v, want 200 %v", code, page.Chats, rev(w.dexChats))
	}
	if !w.marked("ada", w.dm("bea")) || !w.marked("ada", w.dm("fay")) {
		t.Error("ada opened her DMs with bea and fay and they weren't marked read")
	}
	if w.marked("ada", helpers.GetGroupingId(w.people["ada"].String(), ghost.String())) {
		t.Error("a DM with someone who isn't there was marked read")
	}
}

// A message's thread asks the same gate in its own read: a member reads it;
// a non-member and a removed member are refused 403, before anything about
// the message is said; a message that isn't there, or deleted with no
// replies, is a 404.
func TestOpeningAThreadAnswersAsItsGateDoes(t *testing.T) {
	w := newOpenWorld(t)
	thread := func(who, id string) int {
		code, _ := w.get(who, chatController.GetDgraphChatByUUIDWithAllComments, "chat_uuid", id)
		return code
	}
	calWrote := w.groupChats[2]
	cases := []struct {
		who, id, what string
		want          int
	}{
		{"ada", calWrote, "a member", http.StatusOK},
		{"ada", w.dmChats[0], "a member of the DM", http.StatusOK},
		{"eve", calWrote, "a non-member", http.StatusForbidden},
		{"cal", calWrote, "a removed member, on her own message", http.StatusForbidden},
		{"eve", w.dmChats[0], "a non-member, on a DM", http.StatusForbidden},
		{"eve", w.gone, "a non-member, on a deleted message", http.StatusForbidden},
		{"ada", w.gone, "a member, on a deleted message with no replies", http.StatusNotFound},
		{"ada", uuid.NewString(), "a message that isn't there", http.StatusNotFound},
	}
	for _, c := range cases {
		if got := thread(c.who, c.id); got != c.want {
			t.Errorf("%s (%s): %d, want %d", c.what, c.who, got, c.want)
		}
	}
}
