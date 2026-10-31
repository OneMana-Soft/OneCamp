//go:build integration

package business

// B-1: an admin connects a provider with no tokens to paste (Basecamp) by
// signing in at it. The sign-in comes back once, to the admin who began it,
// and its token is tested and kept as a pasted one is. Against Postgres 12
// with every migration, and Redis.
// Run: PKGS=./business/Import integ_rel.sh <wt> <log> 'TestSigningInToAProvider'

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

// signInProvider stands in for Basecamp: connected by signing in, and able
// to say what a token can see.
type signInProvider struct {
	importProvider.Provider
	conn importProvider.OAuthConnect
}

func (s signInProvider) Name() string                              { return importModels.ProviderBasecamp }
func (s signInProvider) OAuthConnect() importProvider.OAuthConnect { return s.conn }
func (s signInProvider) Discover(ctx context.Context, owner string, tok *importModels.Token) ([]importProvider.DiscoverItem, error) {
	if tok.AccessToken != "fresh" {
		return nil, &importProvider.TokenRejected{Msg: "refused"}
	}
	return []importProvider.DiscoverItem{{ID: "999", Name: "Acme", Kind: "account"}}, nil
}

func TestSigningInToAProvider(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	exchanges := 0
	launchpad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh","refresh_token":"keep","expires_in":1209600}`))
	}))
	defer launchpad.Close()
	prov := signInProvider{conn: importProvider.OAuthConnect{Config: importProvider.OAuthConfig{
		ClientID: "id", ClientSecret: "secret",
		Endpoint: oauth2.Endpoint{AuthURL: "https://launchpad.test/authorization/new", TokenURL: launchpad.URL},
	}}}
	// The registry is the process's: Basecamp's own provider comes back for
	// the tests after this one (TestAnOptionalSignInIsOfferedOnlyWhereItWorks
	// read this fake's app).
	basecamp := importProvider.Get(prov.Name())
	importProvider.Register(prov)
	t.Cleanup(func() { importProvider.Register(basecamp) })

	ana, ben := uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{ana, ben} {
		if _, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`, u, u.String()+"@signin.test", u.String(), u.String()); err != nil {
			t.Fatal(err)
		}
	}
	const back = "https://api.test/admin/import/oauth/callback"
	start := func() string {
		t.Helper()
		page, problem := StartOAuthConnect(ctx, prov, ana, back, "")
		if problem != nil {
			t.Fatalf("start: %+v", problem)
		}
		u, _ := url.Parse(page)
		if u.Query().Get("redirect_uri") != back || u.Query().Get("state") == "" {
			t.Fatalf("authorize page %s", page)
		}
		return u.Query().Get("state")
	}

	// Another admin can't finish Ana's sign-in, and the state is spent.
	state := start()
	if _, problem := FinishOAuthConnect(ctx, state, "code", ben, back); problem == nil || problem.Code != "sign_in_expired" {
		t.Fatalf("Ben finished Ana's sign-in: %+v", problem)
	}
	if _, problem := FinishOAuthConnect(ctx, state, "code", ana, back); problem == nil || exchanges != 0 {
		t.Fatalf("a spent state was used (%+v), %d exchanges", problem, exchanges)
	}

	// Ana's own comes back once: tested, saved, and its accounts listed.
	state = start()
	name, problem := FinishOAuthConnect(ctx, state, "code", ana, back)
	if problem != nil || name != importModels.ProviderBasecamp || exchanges != 1 {
		t.Fatalf("finish: %q %+v, %d exchanges", name, problem, exchanges)
	}
	tok, err := importModels.LoadToken(ctx, importModels.ProviderBasecamp, ana)
	if err != nil || tok.AccessToken != "fresh" || tok.RefreshToken != "keep" || tok.ExpiresAt == nil {
		t.Fatalf("kept %+v (%v)", tok, err)
	}
	var items []importProvider.DiscoverItem
	if hit, err := redisStore.GetJSON(ctx, registry.ImportDiscover, []string{importModels.ProviderBasecamp, ana.String()}, &items); err != nil || !hit || len(items) != 1 {
		t.Fatalf("accounts %+v (%v %v)", items, hit, err)
	}
	if _, problem := FinishOAuthConnect(ctx, state, "code", ana, back); problem == nil || exchanges != 1 {
		t.Fatalf("a sign-in came back twice: %+v", problem)
	}

	// A server without the provider's app says how to set it up.
	bare := signInProvider{conn: importProvider.OAuthConnect{Setup: "set it up"}}
	if _, problem := StartOAuthConnect(ctx, bare, ana, back, ""); problem == nil || problem.Code != "not_configured" || problem.Msg != "set it up" {
		t.Fatalf("unconfigured: %+v", problem)
	}
}
