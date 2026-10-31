//go:build integration
// +build integration

package integration_test

// An installed app is an identity of its own (business/App; platform P-API,
// design/platform/API_CONTRACT.md §3.1), against real Postgres, Dgraph and
// Redis: its credential acts as its own user, never the admin who installed
// it, carries no scope the app wasn't given, reaches a table only where the
// app was added, and stops when the app is turned off, rotated away from or
// removed.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnAppsCredential -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	appBusiness "github.com/akashc777/OneCamp/business/App"
	commandBusiness "github.com/akashc777/OneCamp/business/Command"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	tokenModel "github.com/akashc777/OneCamp/models/postgres/ApiToken"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnAppsCredentialActsAsTheAppWhereItWasAddedAndStopsWithIt(t *testing.T) {
	t.Setenv("JWT_SECRET", "app-identity-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	integration.SetupDgraph(t)

	adminID, _, err := userBusiness.CreateUserWithMethod(ctx, "apps-admin@example.test", "Apps Admin", nil, userModels.AuthMethodEmail, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := userBusiness.CreateAdminUser(ctx, "apps-admin@example.test"); err != nil {
		t.Fatal(err)
	}
	actor := dataTableBusiness.Actor{UserID: adminID, IsAdmin: true}
	added, err := dataTableBusiness.CreateTable(ctx, dataTableBusiness.TableInput{Name: "Deploys", Visibility: "workspace"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	other, err := dataTableBusiness.CreateTable(ctx, dataTableBusiness.TableInput{Name: "Payroll", Visibility: "workspace"}, actor)
	if err != nil {
		t.Fatal(err)
	}

	view, err := commandBusiness.CreateApp(ctx, commandAdapter.CreateAppRequest{Slug: "deploy-bot", Name: "Deploy bot", Kind: "external"}, adminID)
	if err != nil {
		t.Fatal(err)
	}
	appID := uuid.MustParse(view.ID)
	if _, err := appBusiness.SetAccess(ctx, appID, []string{"tables:read"},
		appBusiness.Audience{Places: []appBusiness.Place{{Kind: "table", ID: added.Id.String()}}}, ""); err != nil {
		t.Fatal(err)
	}

	// A scope the app wasn't given is refused before anything is minted.
	if _, err := appBusiness.MintToken(ctx, appID, "ci", []string{"tables:write"}, 0); err == nil {
		t.Fatal("a token with a scope beyond the app's own was made")
	}
	made, err := appBusiness.MintToken(ctx, appID, "ci", []string{"tables:read"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := made.Plaintext[:7]; got != "oc_app_" {
		t.Errorf("an app's token begins %q", got)
	}

	// It acts as the app's own user, never the admin who installed it.
	auth, err := apiTokenBusiness.Validate(ctx, made.Plaintext)
	if err != nil || auth == nil {
		t.Fatalf("the app's token didn't validate: %v", err)
	}
	if auth.AppID == nil || *auth.AppID != appID || auth.UserID == adminID {
		t.Fatalf("the token acts as %s for app %v; the admin is %s", auth.UserID, auth.AppID, adminID)
	}

	routes := router.Routes()
	call := func(token, path string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	code, me := call(made.Plaintext, "/v1/me")
	data, _ := me["data"].(map[string]any)
	if code != http.StatusOK || data["id"] != auth.UserID.String() || data["is_admin"] != false {
		t.Errorf("/v1/me as the app answered %d %v", code, me)
	}
	// The app's own user holds only its app's credentials: a plain token it
	// held, or the app's credential held by any other bot, acts for nobody.
	// "oc_": a token's own prefix, so it is refused for its owner, not its form.
	plainHash := "oc_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := tokenModel.CreateToken(ctx, "stray", apiTokenBusiness.HashToken(plainHash), "oc_stray", `["tables:read"]`, auth.UserID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if code, out := call(plainHash, "/v1/me"); code != http.StatusUnauthorized {
		t.Errorf("a plain token held by the app's user answered %d %v", code, out)
	}
	otherBot, err := userBusiness.EnsureAppBot(ctx, uuid.New(), "Another app", "")
	if err != nil {
		t.Fatal(err)
	}
	borrowed := "oc_app_" + uuid.NewString()
	if _, err := tokenModel.CreateAppToken(ctx, "borrowed", apiTokenBusiness.HashToken(borrowed), "oc_app_bor", `["tables:read"]`, otherBot.UserID, nil, appID); err != nil {
		t.Fatal(err)
	}
	if code, out := call(borrowed, "/v1/me"); code != http.StatusUnauthorized {
		t.Errorf("the app's credential held by another bot answered %d %v", code, out)
	}

	errorCode := func(out map[string]any) any {
		e, _ := out["error"].(map[string]any)
		return e["code"]
	}
	if code, out := call(made.Plaintext, "/v1/tables/"+other.Id.String()); code != http.StatusForbidden || errorCode(out) != "not_in_audience" {
		t.Errorf("a table the app wasn't added to answered %d %v", code, out)
	}
	if _, out := call(made.Plaintext, "/v1/tables/"+added.Id.String()); errorCode(out) == "not_in_audience" {
		t.Errorf("the table the app was added to was refused: %v", out)
	}

	// Turned off, every credential stops at once; turned on, it works again.
	if err := commandBusiness.SetAppEnabled(ctx, appID, false); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(made.Plaintext, "/v1/me"); code != http.StatusUnauthorized {
		t.Errorf("with the app off, its token answered %d", code)
	}
	if err := commandBusiness.SetAppEnabled(ctx, appID, true); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(made.Plaintext, "/v1/me"); code != http.StatusOK {
		t.Errorf("with the app back on, its token answered %d", code)
	}

	// Rotated: the same credential with a new secret; the old one is dead.
	fresh, err := appBusiness.RotateToken(ctx, appID, made.Token.Id)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := apiTokenBusiness.Validate(ctx, made.Plaintext); a != nil {
		t.Error("the old secret still works after rotation")
	}
	if a, _ := apiTokenBusiness.Validate(ctx, fresh); a == nil || a.TokenID != made.Token.Id {
		t.Errorf("the new secret isn't the same credential: %+v", a)
	}

	// Taking a scope from the app takes it from its tokens on their next call,
	// and giving it back gives it back.
	places := appBusiness.Audience{Places: []appBusiness.Place{{Kind: "table", ID: added.Id.String()}}}
	if _, err := appBusiness.SetAccess(ctx, appID, []string{"tasks:read"}, places, ""); err != nil {
		t.Fatal(err)
	}
	if a, _ := apiTokenBusiness.Validate(ctx, fresh); a == nil || len(a.Scopes) != 0 {
		t.Errorf("with tables:read taken from the app, its token carries %+v", a)
	}
	if code, out := call(fresh, "/v1/tables/"+added.Id.String()); code != http.StatusForbidden {
		t.Errorf("a token whose scope the app lost answered %d %v", code, out)
	}
	if _, err := appBusiness.SetAccess(ctx, appID, []string{"tables:read"}, places, ""); err != nil {
		t.Fatal(err)
	}
	if a, _ := apiTokenBusiness.Validate(ctx, fresh); a == nil || len(a.Scopes) != 1 || a.Scopes[0] != "tables:read" {
		t.Errorf("with tables:read given back, its token carries %+v", a)
	}

	// Admin › Apps describes its places. Tables decide access (business/
	// DataTable registers the kind), so its row says whether the app's user
	// opens the table, and it says what the app's own token finds when it
	// asks: a promise in the admin screen, kept by the API. The name is the
	// one the Access Resolver gives this admin (business/Access titles), so
	// the row never names what the admin couldn't read.
	identity, err := appBusiness.Get(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	admin := accessBusiness.Subject{UserUUID: adminID.String()}
	described, err := appBusiness.DescribePlaces(ctx, admin, identity)
	if err != nil || len(described.Places) != 1 || described.Places[0].ID != added.Id.String() || described.Places[0].AppOpens == nil {
		t.Fatalf("its places described: %+v, %v", described, err)
	}
	code, _ = call(fresh, "/v1/tables/"+added.Id.String())
	if opens := *described.Places[0].AppOpens; opens != (code == http.StatusOK) {
		t.Errorf("Admin › Apps says the app opens the table: %v; its token got %d", opens, code)
	}
	ref := accessBusiness.ItemRef{Kind: "table", ID: added.Id.String()}
	titles, err := accessBusiness.ReadableTitles(ctx, []accessBusiness.Subject{admin}, []accessBusiness.ItemRef{ref})
	if err != nil || described.Places[0].Title != titles[ref] {
		t.Errorf("the place is named %q; the admin may read it as %q (%v)", described.Places[0].Title, titles[ref], err)
	}

	// Removed: its credentials are revoked with it.
	if err := commandBusiness.DeleteApp(ctx, appID); err != nil {
		t.Fatal(err)
	}
	if a, _ := apiTokenBusiness.Validate(ctx, fresh); a != nil {
		t.Error("a removed app's token still works")
	}
	if _, err := appBusiness.MintToken(ctx, appID, "after", []string{"tables:read"}, 0); err == nil {
		t.Error("a removed app was made a token")
	}
}
