//go:build integration
// +build integration

package integration_test

// V-1191: the read of a person by uuid answers "no one" as not-found
// (helpers.ErrNotFound), never as a zero person with no error. A zero person
// passed every caller's nil check, so an unknown uuid walked into writes (a
// chat notification setting for nobody), a DM room of nobody was taken as
// this workspace's, and a deactivated person read SLA state as a member.
// What every other caller did with a missing person is pinned here: a
// session for nobody is still refused (401, never 503), a display still
// renders with an empty name, and the store calls stay what they were.
//
// Run: PKGS=./tests/integration/ integ_rel.sh <wt> <log> 'NobodyByUUID'

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userChatNotificationBusiness "github.com/akashc777/OneCamp/business/UserChatNotification"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/authcookie"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	postgresModels "github.com/akashc777/OneCamp/models/postgres"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// nobodyWorld is a workspace with one person of every kind: ada and bea,
// members; gus, a guest; bo, an integration's bot; ai, an AI teammate's principal;
// pat, a person an import made a placeholder for; dex, a deactivated member.
// ghost is a uuid nobody here has.
type nobodyWorld struct {
	ctx    context.Context
	env    *integration.Env
	taps   *integration.StoreTaps
	people map[string]uuid.UUID
	ghost  uuid.UUID
}

func newNobodyWorld(t *testing.T) *nobodyWorld {
	t.Helper()
	t.Setenv("JWT_SECRET", "nobody-by-uuid-integration-secret")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	search := integration.SetupOpenSearch(t)
	t.Cleanup(func() { helpers.SettleBackground(10 * time.Second) })

	w := &nobodyWorld{ctx: ctx, env: env, people: map[string]uuid.UUID{}, ghost: uuid.New()}
	for _, p := range []struct {
		name, email   string
		external, bot bool
	}{
		{"ada", "ada@acme.test", false, false},
		{"bea", "bea@acme.test", false, false},
		{"gus", "gus@elsewhere.test", true, false},
		{"bo", userDomain.IntegrationBotPrefix + "bo" + userDomain.BotEmailDomain, true, true},
		{"ai", userDomain.AgentBotPrefix + "ai" + userDomain.BotEmailDomain, true, true},
		{"pat", "pat@placeholder.acme.test", true, false},
		{"dex", "dex@acme.test", false, false},
	} {
		id := uuid.New()
		w.people[p.name] = id
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, is_external, is_bot, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())`, id, p.email, p.name, p.name, p.external, p.bot); err != nil {
			t.Fatal(err)
		}
		dg.Mutate(t, map[string]any{"uid": "_:u", "dgraph.type": "User", "user_uuid": id.String(), "user_name": p.name})
	}
	if _, err := env.PG.Exec(`UPDATE users SET deleted_at = NOW() WHERE id = $1`, w.people["dex"]); err != nil {
		t.Fatal(err)
	}
	w.taps = integration.TapStores(t, env, dg, search)
	return w
}

// as is ctx with name signed in, as the auth middleware leaves it.
func (w *nobodyWorld) as(t *testing.T, name string) context.Context {
	t.Helper()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(w.ctx, w.people[name])
	if err != nil || pg == nil {
		t.Fatalf("load %s: %v", name, err)
	}
	gr, err := userDomain.GetDgraphUserInfoByUUID(w.ctx, w.people[name].String())
	if err != nil || gr == nil {
		t.Fatalf("load %s's node: %v", name, err)
	}
	return context.WithValue(w.ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr})
}

// measured runs do and logs its store calls as a STORE line (the head's
// before and after numbers come from these lines in the red and green runs).
func (w *nobodyWorld) measured(t *testing.T, what string, do func()) integration.Window {
	t.Helper()
	got, err := w.taps.Measure(do)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("STORE %s: sync %v, async %v", what, got.Sync, got.Async)
	return got
}

func TestNobodyByUUIDIsNotFound(t *testing.T) {
	w := newNobodyWorld(t)

	u, err := userDomain.GetUserByUUID(w.ctx, w.ghost)
	if !errors.Is(err, helpers.ErrNotFound) || u != nil {
		t.Errorf("the read of nobody: %+v, %v; want nil and a not-found", u, err)
	}
	u, err = userBusiness.GetUserByUUID(w.ctx, w.ghost)
	if !errors.Is(err, helpers.ErrNotFound) || u != nil {
		t.Errorf("the business read of nobody: %+v, %v; want nil and the not-found passed on", u, err)
	}
	u, err = userDomain.GetActiveUserByUUID(w.ctx, w.people["dex"])
	if !errors.Is(err, helpers.ErrNotFound) || u != nil {
		t.Errorf("the active read of dex, deactivated: %+v, %v; want nil and a not-found", u, err)
	}

	// A deactivated person is still someone: the plain read finds them, as
	// it always has (it doesn't filter deleted_at).
	u, err = userDomain.GetUserByUUID(w.ctx, w.people["dex"])
	if err != nil || u == nil || u.Id != w.people["dex"] || u.DeletedAt.IsZero() {
		t.Errorf("the read of dex, deactivated: %+v, %v; want dex with his deletion time", u, err)
	}
	u, err = userDomain.GetActiveUserByUUID(w.ctx, w.people["ada"])
	if err != nil || u == nil || u.Id != w.people["ada"] {
		t.Errorf("the active read of ada: %+v, %v; want ada", u, err)
	}
	if userDomain.IsAgentPrincipal(w.ctx, w.ghost) || !userDomain.IsAgentPrincipal(w.ctx, w.people["ai"]) {
		t.Errorf("agent principals: nobody %v, ai %v; want false, true",
			userDomain.IsAgentPrincipal(w.ctx, w.ghost), userDomain.IsAgentPrincipal(w.ctx, w.people["ai"]))
	}
}

// A chat notification setting is for a DM with someone: one for nobody is
// refused before its write (an UPDATE that matched no row and answered 200).
// The DM page with nobody stays a 400 (H5B).
func TestNobodyByUUIDGetsNoChatNotificationRow(t *testing.T) {
	w := newNobodyWorld(t)
	ada := w.as(t, "ada")
	set := func(to string) int {
		t.Helper()
		body := `{"notification_type":"all","to_user_id":"` + to + `"}`
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body)).WithContext(ada)
		rec := httptest.NewRecorder()
		userController.UpdateUserChatNotification(rec, req)
		return rec.Code
	}
	rows := func() int {
		t.Helper()
		var n int
		if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM users_chat_notification`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	withBea := helpers.GetGroupingId(w.people["ada"].String(), w.people["bea"].String())
	kind := func() string {
		t.Helper()
		var k string
		if err := w.env.PG.QueryRow(`SELECT notification_type FROM users_chat_notification WHERE user_id = $1 AND grp_id = $2`, w.people["ada"], withBea).Scan(&k); err != nil {
			t.Fatal(err)
		}
		return k
	}
	// The settings a DM gets when it starts, one for each of its two people (a
	// setting changes one, never adds one).
	userChatNotificationBusiness.CreateChatNotificationType(w.ctx, w.people["ada"].String(), w.people["bea"].String(), withBea, postgresModels.NOTIFICATION_TYPE_MENTION, time.Now())
	if n := rows(); n != 2 {
		t.Fatalf("%d notification rows after ada's DM with bea started, want 2", n)
	}

	var code int
	unknown := w.measured(t, "UpdateUserChatNotification unknown to_user_id", func() { code = set(w.ghost.String()) })
	if code != http.StatusBadRequest {
		t.Errorf("a notification setting for nobody: %d, want 400", code)
	}
	if pg := unknown.Sync[integration.StorePostgres]; pg != 1 {
		t.Errorf("a notification setting for nobody made %d Postgres calls, want 1 (the person read, no write)", pg)
	}
	if n := rows(); n != 2 {
		t.Errorf("%d notification rows after a setting for nobody, want 2 (the DM with bea's)", n)
	}
	w.measured(t, "UpdateUserChatNotification known to_user_id", func() { code = set(w.people["bea"].String()) })
	if code != http.StatusOK {
		t.Errorf("a notification setting for bea: %d, want 200", code)
	}
	if k := kind(); k != postgresModels.NOTIFICATION_TYPE_ALL {
		t.Errorf("ada's setting for her DM with bea: %q, want %q", k, postgresModels.NOTIFICATION_TYPE_ALL)
	}

	rc := chi.NewRouteContext()
	rc.URLParams.Add("user_uuid", w.ghost.String())
	rec := httptest.NewRecorder()
	w.measured(t, "dm latestChat unknown counterpart", func() {
		chatController.GetLatestChats(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(ada, chi.RouteCtxKey, rc)))
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("ada's DM page with nobody: %d, want 400", rec.Code)
	}
}

// A session is refused for nobody and for a deactivated person, as it always
// was: 401, never 503 (a 503 is "Postgres didn't answer", and the web keeps
// retrying a refresh it gets). Signing out passes through either way; only a
// person who is here (deactivated or not) is in its context.
func TestNobodyByUUIDHasNoSession(t *testing.T) {
	w := newNobodyWorld(t)
	type seen struct {
		code int
		who  uuid.UUID
	}
	serve := func(mw func(http.Handler) http.Handler, method, cookie, typ string, id uuid.UUID) seen {
		t.Helper()
		token, err := helpers.SignSessionToken(id.String(), typ, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, "/", nil)
		req.AddCookie(&http.Cookie{Name: cookie, Value: token})
		rec := httptest.NewRecorder()
		var who uuid.UUID
		mw(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if u, ok := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo); ok {
				who = u.UserPostgresInfo.Id
			}
			rw.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		return seen{rec.Code, who}
	}
	people := []struct {
		name string
		id   uuid.UUID
	}{{"nobody", w.ghost}, {"dex, deactivated", w.people["dex"]}, {"ada", w.people["ada"]}}
	routes := []struct {
		route               string
		mw                  func(http.Handler) http.Handler
		method, cookie, typ string
		want                []seen
	}{
		{"GET /refreshToken (VerifyRefreshToken)", customMiddleware.VerifyRefreshToken, http.MethodGet, authcookie.Refresh, helpers.TokenTypeRefresh,
			[]seen{{http.StatusUnauthorized, uuid.Nil}, {http.StatusUnauthorized, uuid.Nil}, {http.StatusOK, w.people["ada"]}}},
		{"GET /basicSelfProfile (VerifyAuthOnlyPostgres)", customMiddleware.VerifyAuthOnlyPostgres, http.MethodGet, authcookie.Access, helpers.TokenTypeAccess,
			[]seen{{http.StatusUnauthorized, uuid.Nil}, {http.StatusUnauthorized, uuid.Nil}, {http.StatusOK, w.people["ada"]}}},
		{"POST /logout (VerifyRefreshTokenForLogout)", customMiddleware.VerifyRefreshTokenForLogout, http.MethodPost, authcookie.Refresh, helpers.TokenTypeRefresh,
			[]seen{{http.StatusOK, uuid.Nil}, {http.StatusOK, w.people["dex"]}, {http.StatusOK, w.people["ada"]}}},
	}
	for _, r := range routes {
		for i, p := range people {
			if got := serve(r.mw, r.method, r.cookie, r.typ, p.id); got != r.want[i] {
				t.Errorf("%s as %s: %d with %v in its context, want %d with %v", r.route, p.name, got.code, got.who, r.want[i].code, r.want[i].who)
			}
		}
	}
}

// Who reads a project as a guest (its SLA state and alerts are members'):
// every kind of person reads as they did, and someone with no active row
// (deactivated, or nobody) reads as a guest, as ReadsAsGuest's doc says.
func TestNobodyByUUIDReadsAsAGuest(t *testing.T) {
	w := newNobodyWorld(t)
	for _, c := range []struct {
		who, id string
		guest   bool
	}{
		{"ada, a member", w.people["ada"].String(), false},
		{"an API token acting as ada (her id)", w.people["ada"].String(), false},
		{"gus, a guest", w.people["gus"].String(), true},
		{"bo, an integration's bot", w.people["bo"].String(), false},
		{"ai, an AI teammate's principal", w.people["ai"].String(), false},
		{"pat, an import's placeholder", w.people["pat"].String(), true},
		{"dex, deactivated", w.people["dex"].String(), true},
		{"nobody", w.ghost.String(), true},
		{"not a uuid", "not-a-uuid", true},
	} {
		if got := userDomain.ReadsAsGuest(w.ctx, c.id); got != c.guest {
			t.Errorf("%s reads as a guest: %v, want %v", c.who, got, c.guest)
		}
	}
}

// A shared LiveKit server tells every workspace about every room: a DM room
// is this workspace's when one of its two people is here, not when its name
// merely parses.
func TestNobodyByUUIDsDMRoomIsNotOurs(t *testing.T) {
	w := newNobodyWorld(t)
	room := func(a, b uuid.UUID) string {
		return callStateBusiness.RoomOf(conversationBusiness.ChatRef(helpers.GetGroupingId(a.String(), b.String())))
	}
	if callStateBusiness.BelongsHere(w.ctx, room(w.ghost, uuid.New())) {
		t.Errorf("a DM room of two people who aren't here is taken as ours")
	}
	if !callStateBusiness.BelongsHere(w.ctx, room(w.people["ada"], uuid.New())) {
		t.Errorf("a DM room of ada's with someone elsewhere isn't ours")
	}
	if !callStateBusiness.BelongsHere(w.ctx, room(w.people["ada"], w.people["bea"])) {
		t.Errorf("ada's DM room with bea isn't ours")
	}
}

// An audit row names its actor by address: a missing actor's row still
// renders, with no address, as it always did.
func TestNobodyByUUIDsAuditRowRenders(t *testing.T) {
	w := newNobodyWorld(t)
	var email string
	w.measured(t, "audit event actor email missing actor", func() { email = auditBusiness.EventActorEmail(w.ctx, w.ghost) })
	if email != "" {
		t.Errorf("a missing actor's address: %q, want empty", email)
	}
	if got := auditBusiness.EventActorEmail(w.ctx, w.people["ada"]); got != "ada@acme.test" {
		t.Errorf("ada's address: %q, want ada@acme.test", got)
	}
}
