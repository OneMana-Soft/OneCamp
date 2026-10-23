//go:build integration
// +build integration

package integration_test

// An installed app is held to the places it was added to on every /v1 route
// (business/App; A10), against real Postgres, Dgraph, Redis and OpenSearch,
// through the real router. Added to #deploys, its token posts there. It can't
// post in #deploys-payroll or find it in a search, though its own user is in
// both channels, and it can't message a person or read a person's unread
// counts. A failed read of the app is business/PublicAPI's unit test's.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnAppIsHeldToItsPlaces -v

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	appBusiness "github.com/akashc777/OneCamp/business/App"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	commandBusiness "github.com/akashc777/OneCamp/business/Command"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

// appInOneChannel is an app added to #deploys alone, whose own user is in
// #deploys and #deploys-payroll, with a token for messages, search and unread
// counts; and the admin who made it all.
type appInOneChannel struct {
	t                *testing.T
	env              *integration.Env
	admin            uuid.UUID
	deploys, payroll string
	token            string
}

func seedAppInOneChannel(t *testing.T) *appInOneChannel {
	t.Helper()
	t.Setenv("JWT_SECRET", "app-places-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}

	adminID, _, err := userBusiness.CreateUserWithMethod(ctx, "apps-admin@example.test", "Apps Admin", nil, userModels.AuthMethodEmail, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := userBusiness.CreateAdminUser(ctx, "apps-admin@example.test"); err != nil {
		t.Fatal(err)
	}
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, adminID)
	if err != nil || pg == nil {
		t.Fatalf("load the admin: %v", err)
	}
	node, err := userDomain.GetDgraphUserInfoByUUID(ctx, adminID.String())
	if err != nil || node == nil {
		t.Fatalf("load the admin's node: %v", err)
	}
	admin := userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *node}
	channel := func(name string) string {
		err, id := channelBusiness.CreateChannel(ctx, &channelAdapter.InputCreateChannel{ChannelName: name}, &admin)
		if err != nil {
			t.Fatalf("make #%s: %v", name, err)
		}
		return id.String()
	}
	fx := &appInOneChannel{t: t, env: env, admin: adminID, deploys: channel("deploys"), payroll: channel("deploys-payroll")}

	view, err := commandBusiness.CreateApp(ctx, commandAdapter.CreateAppRequest{Slug: "deploy-bot", Name: "Deploy bot", Kind: "external"}, adminID)
	if err != nil {
		t.Fatal(err)
	}
	appID := uuid.MustParse(view.ID)
	scopes := []string{"messages:write", "search:read", "attention:read"}
	if _, err := appBusiness.SetAccess(ctx, appID, scopes,
		appBusiness.Audience{Places: []appBusiness.Place{{Kind: "channel", ID: fx.deploys}}}); err != nil {
		t.Fatal(err)
	}
	made, err := appBusiness.MintToken(ctx, appID, "ci", scopes, 0)
	if err != nil {
		t.Fatal(err)
	}
	fx.token = made.Plaintext

	// The app's own user is in both channels, both ways, as the app writes it:
	// what holds it to #deploys is its places, not its membership.
	identity, err := appBusiness.Get(ctx, appID)
	if err != nil || identity.BotUserID == nil {
		t.Fatalf("the app's user: %+v, %v", identity, err)
	}
	var got struct {
		U []struct {
			UID string `json:"uid"`
		} `json:"u"`
		C []struct {
			UID string `json:"uid"`
		} `json:"c"`
	}
	raw := dg.Query(t, `{ u(func: eq(user_uuid, "`+identity.BotUserID.String()+`")) { uid }
		c(func: eq(ch_uuid, ["`+fx.deploys+`", "`+fx.payroll+`"])) { uid } }`)
	if err := json.Unmarshal(raw, &got); err != nil || len(got.U) != 1 || len(got.C) != 2 {
		t.Fatalf("the app's user and the channels in the graph: %v %s", err, raw)
	}
	edges := []map[string]any{{"uid": got.U[0].UID, "user_channels": []map[string]any{{"uid": got.C[0].UID}, {"uid": got.C[1].UID}}}}
	for _, c := range got.C {
		edges = append(edges, map[string]any{"uid": c.UID, "ch_members": []map[string]any{{"uid": got.U[0].UID}}})
	}
	dg.Mutate(t, edges)
	return fx
}

// call sends a request with the app's token through the real router.
func (fx *appInOneChannel) call(method, path string, body any) (int, map[string]any) {
	fx.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer "+fx.token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.Routes().ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// refusal is the code and words of the API's one error body.
func refusal(out map[string]any) (code, message any) {
	e, _ := out["error"].(map[string]any)
	return e["code"], e["message"]
}

func TestAnAppIsHeldToItsPlaces(t *testing.T) {
	fx := seedAppInOneChannel(t)

	if code, out := fx.call("POST", "/v1/messages/channel", map[string]any{"channel_uuid": fx.deploys, "text": "Deployed v2.75.2"}); code != http.StatusOK {
		t.Fatalf("posting in the channel it was added to: %d %v", code, out)
	}
	code, out := fx.call("POST", "/v1/messages/channel", map[string]any{"channel_uuid": fx.payroll, "text": "Deployed v2.75.2"})
	if c, _ := refusal(out); code != http.StatusForbidden || c != "not_in_audience" {
		t.Errorf("posting in a channel it wasn't added to: %d %v", code, out)
	}
	var posted int
	if err := fx.env.PG.QueryRow(`SELECT count(*) FROM posts WHERE post_channel = $1`, fx.payroll).Scan(&posted); err != nil || posted != 0 {
		t.Errorf("the refused post: %d in #deploys-payroll (%v)", posted, err)
	}

	// Search finds the channel it was added to, never the other.
	channelsFound := func() string {
		_, out := fx.call("GET", "/v1/search?q=deploys", nil)
		data, _ := out["data"].(map[string]any)
		raw, _ := data["structured_json"].(string)
		var page struct {
			Page []struct {
				Channel *struct {
					ID string `json:"ch_id"`
				} `json:"channel"`
			} `json:"page"`
		}
		_ = json.Unmarshal([]byte(raw), &page)
		var ids []string
		for _, h := range page.Page {
			if h.Channel != nil {
				ids = append(ids, h.Channel.ID)
			}
		}
		sort.Strings(ids)
		return strings.Join(ids, ",")
	}
	found := ""
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if found = channelsFound(); found != "" {
			break
		}
	}
	if found != fx.deploys {
		t.Errorf("a search for \"deploys\" found channels [%s], want #deploys alone (%s)", found, fx.deploys)
	}

	// No place it is added to is a person, and unread counts are a person's.
	for _, c := range []struct {
		name, method, path string
		body               any
		words              string
	}{
		{"a direct message", "POST", "/v1/messages/dm", map[string]any{"to_uuid": fx.admin.String(), "text": "hi"}, "This app can't message people directly."},
		{"unread counts", "GET", "/v1/unread", nil, "Unread counts are a person's; an app has none."},
	} {
		code, out := fx.call(c.method, c.path, c.body)
		if got, words := refusal(out); code != http.StatusForbidden || got != "not_for_apps" || words != c.words {
			t.Errorf("%s: %d %v, want 403 not_for_apps %q", c.name, code, out, c.words)
		}
	}
	var dms int
	if err := fx.env.PG.QueryRow(`SELECT count(*) FROM chats WHERE grp_id LIKE '%' || $1 || '%'`, fx.admin.String()).Scan(&dms); err != nil || dms != 0 {
		t.Errorf("the refused DM: %d chats with the admin (%v)", dms, err)
	}
}
