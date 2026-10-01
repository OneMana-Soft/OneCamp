package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	agentBusiness "github.com/akashc777/OneCamp/business/AIAgent"
	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	tokenModel "github.com/akashc777/OneCamp/models/postgres/ApiToken"
	model "github.com/akashc777/OneCamp/models/postgres/OAuth"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/google/uuid"
)

// fakeStore is the database, in memory, with the same once-only semantics.
type fakeStore struct {
	mu       sync.Mutex
	clients  map[uuid.UUID]*model.Client
	requests map[uuid.UUID]*model.Request
	codes    map[string]*model.Code
	grants   map[uuid.UUID]*fakeGrant
	tokens   map[uuid.UUID]*fakeToken
	agents   map[uuid.UUID]*agentModel.AiAgent
	surface  bool
}

type fakeGrant struct {
	g       model.Grant
	refresh string
	revoked bool
}

type fakeToken struct {
	secret  string
	scopes  []string
	agent   *uuid.UUID
	revoked bool
	expires time.Time
}

func install(t *testing.T) *fakeStore {
	t.Helper()
	f := &fakeStore{
		clients: map[uuid.UUID]*model.Client{}, requests: map[uuid.UUID]*model.Request{},
		codes: map[string]*model.Code{}, grants: map[uuid.UUID]*fakeGrant{},
		tokens: map[uuid.UUID]*fakeToken{}, agents: map[uuid.UUID]*agentModel.AiAgent{}, surface: true,
	}
	saved := []interface{}{createClient, getClient, createRequest, getRequest, takeRequest, createCode, takeCode,
		createGrant, grantByRefresh, grantByToken, rotateRefresh, revokeGrant, liveGrantsFor, mintToken, rotateSecret,
		revokeToken, tokenByHash, listAgents, createAgent, getAgent, userIsAdmin, surfaceEnabled}
	t.Cleanup(func() {
		createClient = saved[0].(func(context.Context, string, []string) (uuid.UUID, error))
		getClient = saved[1].(func(context.Context, uuid.UUID) (*model.Client, error))
		createRequest = saved[2].(func(context.Context, *model.Request) (uuid.UUID, error))
		getRequest = saved[3].(func(context.Context, uuid.UUID) (*model.Request, error))
		takeRequest = saved[4].(func(context.Context, uuid.UUID) (*model.Request, error))
		createCode = saved[5].(func(context.Context, *model.Code) error)
		takeCode = saved[6].(func(context.Context, string) (*model.Code, error))
		createGrant = saved[7].(func(context.Context, *model.Grant, string) error)
		grantByRefresh = saved[8].(func(context.Context, string) (*model.Grant, error))
		grantByToken = saved[9].(func(context.Context, uuid.UUID) (*model.Grant, error))
		rotateRefresh = saved[10].(func(context.Context, uuid.UUID, string, string, time.Time) (bool, error))
		revokeGrant = saved[11].(func(context.Context, uuid.UUID) (*uuid.UUID, error))
		liveGrantsFor = saved[12].(func(context.Context, uuid.UUID, uuid.UUID, string) ([]uuid.UUID, error))
		mintToken = saved[13].(func(context.Context, string, []string, *time.Time, uuid.UUID, *uuid.UUID, bool) (*apiTokenBusiness.CreatedToken, error))
		rotateSecret = saved[14].(func(context.Context, uuid.UUID, time.Time) (string, error))
		revokeToken = saved[15].(func(context.Context, uuid.UUID) error)
		tokenByHash = saved[16].(func(context.Context, string) (*tokenModel.ApiToken, error))
		listAgents = saved[17].(func(context.Context, agentBusiness.Actor) ([]*agentModel.AiAgent, error))
		createAgent = saved[18].(func(context.Context, agentBusiness.AgentInput, uuid.UUID) (*agentModel.AiAgent, error))
		getAgent = saved[19].(func(context.Context, uuid.UUID) (*agentModel.AiAgent, error))
		userIsAdmin = saved[20].(func(context.Context, uuid.UUID) (bool, bool))
		surfaceEnabled = saved[21].(func(context.Context) bool)
	})

	createClient = func(_ context.Context, name string, uris []string) (uuid.UUID, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := uuid.New()
		f.clients[id] = &model.Client{ID: id, Name: name, RedirectURIs: uris}
		return id, nil
	}
	getClient = func(_ context.Context, id uuid.UUID) (*model.Client, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.clients[id], nil
	}
	createRequest = func(_ context.Context, r *model.Request) (uuid.UUID, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r.ID = uuid.New()
		cp := *r
		f.requests[r.ID] = &cp
		return r.ID, nil
	}
	getRequest = func(_ context.Context, id uuid.UUID) (*model.Request, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r := f.requests[id]; r != nil && r.ExpiresAt.After(now()) {
			return r, nil
		}
		return nil, nil
	}
	takeRequest = func(_ context.Context, id uuid.UUID) (*model.Request, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r := f.requests[id]
		delete(f.requests, id)
		if r == nil || !r.ExpiresAt.After(now()) {
			return nil, nil
		}
		return r, nil
	}
	createCode = func(_ context.Context, c *model.Code) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.codes[c.Hash] = c
		return nil
	}
	takeCode = func(_ context.Context, h string) (*model.Code, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		c := f.codes[h]
		delete(f.codes, h)
		if c == nil || !c.ExpiresAt.After(now()) {
			return nil, nil
		}
		return c, nil
	}
	createGrant = func(_ context.Context, g *model.Grant, refresh string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		g.ID = uuid.New()
		f.grants[g.ID] = &fakeGrant{g: *g, refresh: refresh}
		return nil
	}
	grantByRefresh = func(_ context.Context, h string) (*model.Grant, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, fg := range f.grants {
			if fg.refresh == h && !fg.revoked {
				g := fg.g
				return &g, nil
			}
		}
		return nil, nil
	}
	grantByToken = func(_ context.Context, id uuid.UUID) (*model.Grant, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, fg := range f.grants {
			if fg.g.TokenID == id && !fg.revoked {
				g := fg.g
				return &g, nil
			}
		}
		return nil, nil
	}
	rotateRefresh = func(_ context.Context, id uuid.UUID, old, next string, _ time.Time) (bool, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		fg := f.grants[id]
		if fg == nil || fg.revoked || fg.refresh != old {
			return false, nil
		}
		fg.refresh = next
		return true, nil
	}
	revokeGrant = func(_ context.Context, id uuid.UUID) (*uuid.UUID, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		fg := f.grants[id]
		if fg == nil || fg.revoked {
			return nil, nil
		}
		fg.revoked = true
		t := fg.g.TokenID
		return &t, nil
	}
	liveGrantsFor = func(_ context.Context, user, agent uuid.UUID, name string) ([]uuid.UUID, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []uuid.UUID
		for id, fg := range f.grants {
			if !fg.revoked && fg.g.UserID == user && fg.g.AgentID == agent && f.clients[fg.g.ClientID].Name == name {
				out = append(out, id)
			}
		}
		return out, nil
	}
	mintToken = func(_ context.Context, name string, scopes []string, exp *time.Time, by uuid.UUID, agent *uuid.UUID, _ bool) (*apiTokenBusiness.CreatedToken, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := uuid.New()
		secret := "oc_" + uuid.NewString()
		f.tokens[id] = &fakeToken{secret: secret, scopes: scopes, agent: agent, expires: *exp}
		return &apiTokenBusiness.CreatedToken{Token: &tokenModel.ApiToken{Id: id, Name: name, CreatedBy: by, AgentId: agent}, Plaintext: secret}, nil
	}
	rotateSecret = func(_ context.Context, id uuid.UUID, exp time.Time) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		tk := f.tokens[id]
		if tk == nil || tk.revoked {
			return "", errors.New("gone")
		}
		tk.secret = "oc_" + uuid.NewString()
		tk.expires = exp
		return tk.secret, nil
	}
	revokeToken = func(_ context.Context, id uuid.UUID) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if tk := f.tokens[id]; tk != nil {
			tk.revoked = true
		}
		return nil
	}
	tokenByHash = func(_ context.Context, h string) (*tokenModel.ApiToken, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for id, tk := range f.tokens {
			if !tk.revoked && apiTokenBusiness.HashToken(tk.secret) == h {
				return &tokenModel.ApiToken{Id: id}, nil
			}
		}
		return nil, nil
	}
	listAgents = func(_ context.Context, actor agentBusiness.Actor) ([]*agentModel.AiAgent, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []*agentModel.AiAgent
		for _, a := range f.agents {
			out = append(out, a)
		}
		return out, nil
	}
	createAgent = func(_ context.Context, in agentBusiness.AgentInput, by uuid.UUID) (*agentModel.AiAgent, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		a := &agentModel.AiAgent{Id: uuid.New(), Name: in.Name, CreatedBy: by, IsActive: in.IsActive}
		b, _ := jsonMarshal(in.EnabledTools)
		a.EnabledTools = b
		f.agents[a.Id] = a
		return a, nil
	}
	getAgent = func(_ context.Context, id uuid.UUID) (*agentModel.AiAgent, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.agents[id], nil
	}
	userIsAdmin = func(context.Context, uuid.UUID) (bool, bool) { return false, true }
	surfaceEnabled = func(context.Context) bool { return f.surface }
	return f
}

func pkcePair() (verifier, challenge string) {
	verifier = strings.Repeat("v", 20) + uuid.NewString()
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

const redirect = "https://claude.ai/api/mcp/auth_callback"

func register(t *testing.T) string {
	t.Helper()
	out, e := Register(context.Background(), RegisterInput{ClientName: "Claude", RedirectURIs: []string{redirect, "http://localhost/callback"}})
	if e != nil {
		t.Fatal(e)
	}
	return out["client_id"].(string)
}

// startSignIn runs /authorize and returns the pending request id.
func startSignIn(t *testing.T, clientID, challenge string, extra url.Values) uuid.UUID {
	t.Helper()
	q := url.Values{
		"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"xyz"},
		"scope": {"tasks:read tasks:write offline_access"}, "resource": {ResourceURL()},
	}
	for k, v := range extra {
		q[k] = v
	}
	res := Authorize(context.Background(), q)
	u, _ := url.Parse(res.Redirect)
	id, err := uuid.Parse(u.Query().Get("request"))
	if !res.Local || err != nil {
		t.Fatalf("authorize did not park the sign-in: %s", res.Redirect)
	}
	return id
}

func codeFrom(t *testing.T, redirectTo string) string {
	t.Helper()
	u, _ := url.Parse(redirectTo)
	if u.Query().Get("state") != "xyz" || u.Query().Get("iss") != BaseURL() {
		t.Fatalf("the approval redirect lost state or iss: %s", redirectTo)
	}
	return u.Query().Get("code")
}

func TestTheWholeConnection(t *testing.T) {
	f := install(t)
	ctx := context.Background()
	me := agentBusiness.Actor{UserID: uuid.New()}
	clientID := register(t)
	verifier, challenge := pkcePair()
	reqID := startSignIn(t, clientID, challenge, nil)

	view, err := Consent(ctx, reqID, me)
	if err != nil {
		t.Fatal(err)
	}
	if view.ClientName != "Claude" || view.RedirectHost != "claude.ai" || len(view.Scopes) != 2 {
		t.Fatalf("consent view = %+v (offline_access is not ours to show)", view)
	}

	res, err := Approve(ctx, reqID, me, ApproveInput{Scopes: []string{"tasks:read", "projects:write"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.AgentName != "Claude" {
		t.Fatalf("approve = %+v, want a new agent named after the client", res)
	}
	agent := f.agents[res.AgentID]
	if agent.CreatedBy != me.UserID || strings.Contains(agent.EnabledTools, "create_task") || !strings.Contains(agent.EnabledTools, "list_tasks") {
		t.Fatalf("the agent's tools must be exactly what tasks:read reaches (projects:write was never asked): %s", agent.EnabledTools)
	}
	if _, err := Approve(ctx, reqID, me, ApproveInput{Scopes: []string{"tasks:read"}}); !errors.Is(err, ErrRequestGone) {
		t.Fatalf("a second approval = %v, want gone", err)
	}

	code := codeFrom(t, res.Redirect)
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {redirect}, "code_verifier": {verifier}}
	tok, e := Token(ctx, form)
	if e != nil {
		t.Fatal(e)
	}
	if tok.TokenType != "Bearer" || tok.ExpiresIn != 3600 || tok.Scope != "tasks:read" || !strings.HasPrefix(tok.RefreshToken, refreshPrefix) {
		t.Fatalf("token response = %+v", tok)
	}
	if _, e := Token(ctx, form); e == nil || e.Code != "invalid_grant" {
		t.Fatalf("reusing a code = %v, want invalid_grant", e)
	}

	// Refresh rotates both, and the old refresh token is dead.
	r1, e := Token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}, "client_id": {clientID}})
	if e != nil || r1.AccessToken == tok.AccessToken || r1.RefreshToken == tok.RefreshToken {
		t.Fatalf("refresh = %+v, %v", r1, e)
	}
	if _, e := Token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}}); e == nil {
		t.Fatal("an old refresh token still worked")
	}
	if len(f.tokens) != 1 {
		t.Fatalf("a refresh made a new credential (%d); it must renew the one the inventory lists", len(f.tokens))
	}

	// Revoking the credential from the inventory ends the connection at the next refresh.
	for id := range f.tokens {
		_ = revokeToken(ctx, id)
	}
	if _, e := Token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {r1.RefreshToken}}); e == nil || e.Code != "invalid_grant" {
		t.Fatalf("refresh after revoke = %v, want invalid_grant", e)
	}
	if g, _ := grantByRefresh(ctx, hashSecret(r1.RefreshToken)); g != nil {
		t.Fatal("the grant outlived its revoked credential")
	}
}

func TestReconnectingReplacesTheEarlierConnection(t *testing.T) {
	f := install(t)
	ctx := context.Background()
	me := agentBusiness.Actor{UserID: uuid.New()}
	clientID := register(t)

	connect := func(agentID string) *TokenResponse {
		v, c := pkcePair()
		id := startSignIn(t, clientID, c, nil)
		res, err := Approve(ctx, id, me, ApproveInput{AgentID: agentID, Scopes: []string{"tasks:read"}})
		if err != nil {
			t.Fatal(err)
		}
		tok, e := Token(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {codeFrom(t, res.Redirect)}, "code_verifier": {v}})
		if e != nil {
			t.Fatal(e)
		}
		return tok
	}
	first := connect("")
	var agentID string
	for id := range f.agents {
		agentID = id.String()
	}
	view, _ := Consent(ctx, startSignIn(t, clientID, "c", nil), me)
	if view.SuggestedAgentID == nil || view.SuggestedAgentID.String() != agentID {
		t.Fatalf("a reconnect should suggest the agent made last time: %+v", view)
	}
	connect(agentID)
	if len(f.agents) != 1 {
		t.Fatalf("reconnecting as the same agent made %d agents", len(f.agents))
	}
	if _, e := Token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken}}); e == nil {
		t.Fatal("the replaced connection still refreshes")
	}
}

func TestRefusals(t *testing.T) {
	f := install(t)
	ctx := context.Background()
	me := agentBusiness.Actor{UserID: uuid.New()}
	clientID := register(t)
	verifier, challenge := pkcePair()

	t.Run("an unknown client is told on our page, never at its address", func(t *testing.T) {
		res := Authorize(ctx, url.Values{"client_id": {uuid.NewString()}, "redirect_uri": {"https://evil.example/cb"}})
		if !res.Local || !strings.Contains(res.Redirect, "error=invalid_client") {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("an unregistered redirect is told on our page", func(t *testing.T) {
		res := Authorize(ctx, url.Values{"client_id": {clientID}, "redirect_uri": {"https://evil.example/cb"}})
		if !res.Local || !strings.Contains(res.Redirect, "error=invalid_redirect_uri") {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("PKCE is required", func(t *testing.T) {
		res := Authorize(ctx, url.Values{"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "state": {"s"}})
		if res.Local || !strings.HasPrefix(res.Redirect, redirect) || !strings.Contains(res.Redirect, "error=invalid_request") || !strings.Contains(res.Redirect, "state=s") {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("another resource is refused", func(t *testing.T) {
		res := Authorize(ctx, url.Values{"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"},
			"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "resource": {"https://other.example/mcp"}})
		if !strings.Contains(res.Redirect, "error=invalid_target") {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("a native client may use any loopback port", func(t *testing.T) {
		res := Authorize(ctx, url.Values{"client_id": {clientID}, "redirect_uri": {"http://localhost:53682/callback"}, "response_type": {"code"},
			"code_challenge": {challenge}, "code_challenge_method": {"S256"}})
		if !res.Local || !strings.Contains(res.Redirect, "request=") {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("nothing is granted while an admin has outside agents off", func(t *testing.T) {
		f.surface = false
		defer func() { f.surface = true }()
		id := startSignIn(t, clientID, challenge, nil)
		if _, err := Approve(ctx, id, me, ApproveInput{Scopes: []string{"tasks:read"}}); !errors.Is(err, ErrSurfaceClosed) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a wrong verifier gets no token", func(t *testing.T) {
		id := startSignIn(t, clientID, challenge, nil)
		res, err := Approve(ctx, id, me, ApproveInput{Scopes: []string{"tasks:read"}})
		if err != nil {
			t.Fatal(err)
		}
		_, e := Token(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {codeFrom(t, res.Redirect)}, "code_verifier": {verifier + "x"}})
		if e == nil || e.Code != "invalid_grant" {
			t.Fatalf("got %v", e)
		}
	})
	t.Run("someone else's agent cannot be borrowed", func(t *testing.T) {
		other := &agentModel.AiAgent{Id: uuid.New(), Name: "Theirs", CreatedBy: uuid.New(), IsActive: true}
		f.agents[other.Id] = other
		id := startSignIn(t, clientID, challenge, nil)
		if _, err := Approve(ctx, id, me, ApproveInput{AgentID: other.Id.String(), Scopes: []string{"tasks:read"}}); err == nil {
			t.Fatal("approved as another person's agent")
		}
		view, _ := Consent(ctx, id, agentBusiness.Actor{UserID: me.UserID, IsAdmin: true})
		for _, a := range view.Agents {
			if a.ID == other.Id {
				t.Fatal("an admin was offered a colleague's agent")
			}
		}
	})
	t.Run("denying tells the client", func(t *testing.T) {
		id := startSignIn(t, clientID, challenge, nil)
		to, err := Deny(ctx, id)
		if err != nil || !strings.Contains(to, "error=access_denied") || !strings.Contains(to, "state=xyz") {
			t.Fatalf("deny = %q, %v", to, err)
		}
	})
}

func TestRedirectAddresses(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback":               true,
		"https://chatgpt.com/connector_platform_oauth_redirect": true,
		"http://localhost:3118/callback":                        true,
		"http://127.0.0.1/callback":                             true,
		"http://example.com/callback":                           false,
		"cursor://anysphere.cursor-retrieval/oauth/callback":    true,
		"javascript:alert(1)":                                   false,
		"https://claude.ai/cb#frag":                             false,
		"":                                                      false,
	} {
		if got := validRedirectURI(raw); got != want {
			t.Errorf("validRedirectURI(%q) = %v, want %v", raw, got, want)
		}
	}
	if !redirectMatches("http://localhost/callback", "http://localhost:9999/callback") {
		t.Error("a loopback redirect must match on any port")
	}
	if redirectMatches("https://a.example/cb", "https://a.example:8443/cb") {
		t.Error("a non-loopback redirect must match exactly")
	}
}

func TestPKCE(t *testing.T) {
	// RFC 7636 appendix B.
	if !verifyPKCE("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk", "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM") {
		t.Fatal("the RFC's own example fails")
	}
	if verifyPKCE("short", "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM") {
		t.Fatal("a too-short verifier passed")
	}
}

func TestScopesAskedFor(t *testing.T) {
	if got := parseScopes("openid offline_access"); len(got) != len(apiTokenBusiness.AllScopes) {
		t.Fatalf("asking for nothing we grant should offer everything to choose from, got %v", got)
	}
	if got := parseScopes("tasks:read tasks:read nope"); len(got) != 1 || got[0] != "tasks:read" {
		t.Fatalf("got %v", got)
	}
}

func TestDiscoveryDocumentsAgree(t *testing.T) {
	pr := ProtectedResourceMetadata()
	as := AuthorizationServerMetadata()
	if pr["authorization_servers"].([]string)[0] != as["issuer"] {
		t.Fatal("the resource names an authorization server that is not this issuer")
	}
	if !strings.HasSuffix(pr["resource"].(string), "/v1/mcp") {
		t.Fatalf("resource = %v", pr["resource"])
	}
	if !strings.HasSuffix(authService.MCPResourceMetadataURL(), "/.well-known/oauth-protected-resource/v1/mcp") {
		t.Fatal("the 401 pointer does not name the document served for /v1/mcp")
	}
}

func jsonMarshal(v []string) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// A member ends their own connection from the app: the grant and its access
// token both stop at once. Someone else's connection, or one already ended, is
// refused with the same answer, so the endpoint cannot be used to probe others.
func TestDisconnect(t *testing.T) {
	f := install(t)
	savedOwner := grantOwner
	t.Cleanup(func() { grantOwner = savedOwner })
	grantOwner = func(_ context.Context, id uuid.UUID) (uuid.UUID, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if fg := f.grants[id]; fg != nil && !fg.revoked {
			return fg.g.UserID, nil
		}
		return uuid.Nil, nil
	}

	me, other := uuid.New(), uuid.New()
	tokenID, grantID := uuid.New(), uuid.New()
	f.tokens[tokenID] = &fakeToken{secret: "oc_x"}
	f.grants[grantID] = &fakeGrant{g: model.Grant{ID: grantID, UserID: me, TokenID: tokenID}}

	if err := Disconnect(context.Background(), other, grantID); !errors.Is(err, ErrNotYourConnection) {
		t.Fatalf("someone else's connection: got %v, want ErrNotYourConnection", err)
	}
	if f.grants[grantID].revoked || f.tokens[tokenID].revoked {
		t.Fatal("a refused disconnect must change nothing")
	}
	if err := Disconnect(context.Background(), me, grantID); err != nil {
		t.Fatalf("own connection: %v", err)
	}
	if !f.grants[grantID].revoked || !f.tokens[tokenID].revoked {
		t.Fatal("disconnect must end both the grant and its access token")
	}
	if err := Disconnect(context.Background(), me, grantID); !errors.Is(err, ErrNotYourConnection) {
		t.Fatalf("an ended connection: got %v, want ErrNotYourConnection", err)
	}
}
