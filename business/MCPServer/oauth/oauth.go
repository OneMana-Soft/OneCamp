// Package oauth lets MCP clients sign a person in with OAuth
// 2.1 instead of asking them to paste a token.
//
// WHY THIS EXISTS. Claude, Cowork and ChatGPT add a remote MCP server by URL and
// then sign the person in; most of them have no field for a bearer token at all
// (ChatGPT supports OAuth or nothing, Claude's request-header option is a beta
// for a few organisations). So "connect your agent to OneCamp" failed at the
// first step for most of the people who would try it.
//
// WHAT A CONNECTION IS. Approving a sign-in names an agent identity the person
// sponsors (a new one named after the client, or one they already manage), and
// the client receives an ordinary API credential bound to that agent, renewed
// hourly. Nothing about governance is new: the inventory lists the credential,
// the agent's kill switch stops it on every surface, its scopes and the agent's
// toolset bound it, and the audit log names the agent.
//
// THE PROFILE. Public clients only (no client secret; PKCE with S256 is
// required), dynamic registration (RFC 7591), protected-resource metadata (RFC
// 9728), authorization-server metadata (RFC 8414), refresh-token rotation, and
// revocation (RFC 7009). That is what Claude, ChatGPT, Grok Bot, Cursor and
// Claude Code need; nothing more is offered, because nothing more is used.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	agentBusiness "github.com/akashc777/OneCamp/business/AIAgent"
	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	mcpBusiness "github.com/akashc777/OneCamp/business/MCPServer"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	tokenModel "github.com/akashc777/OneCamp/models/postgres/ApiToken"
	model "github.com/akashc777/OneCamp/models/postgres/OAuth"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/google/uuid"
)

const (
	// AccessTTL is how long an access token lives. Short, because it is the
	// thing that travels; the refresh token renews it without the person.
	AccessTTL = time.Hour
	// RefreshTTL is how long a connection survives unused. Each refresh
	// restarts it, so a client in regular use stays connected.
	RefreshTTL = 90 * 24 * time.Hour
	codeTTL    = 5 * time.Minute
	requestTTL = 10 * time.Minute

	refreshPrefix   = "ocr_"
	maxRedirectURIs = 10
	maxURILen       = 2000
	maxNameLen      = 80
	maxStateLen     = 1024
)

// Error is an OAuth error in RFC 6749 terms.
type Error struct {
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
	// Status is the HTTP status the token and registration endpoints answer
	// with. Not serialised.
	Status int `json:"-"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Description }

func oauthErr(code, desc string) *Error { return &Error{Code: code, Description: desc, Status: 400} }

// ---------------------------------------------------------------------------
// Addresses

// BaseURL is the issuer: this server's public origin.
func BaseURL() string { return authService.BackendBaseURL() }

// ResourceURL is the MCP endpoint, which is what a client asks access to.
func ResourceURL() string { return BaseURL() + "/v1/mcp" }

// ProtectedResourceMetadata is RFC 9728's document for /v1/mcp.
func ProtectedResourceMetadata() map[string]interface{} {
	return map[string]interface{}{
		"resource":                 ResourceURL(),
		"authorization_servers":    []string{BaseURL()},
		"scopes_supported":         apiTokenBusiness.AllScopes,
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "OneCamp",
	}
}

// AuthorizationServerMetadata is RFC 8414's document.
func AuthorizationServerMetadata() map[string]interface{} {
	b := BaseURL()
	return map[string]interface{}{
		"issuer":                                         b,
		"authorization_endpoint":                         b + "/oauth/authorize",
		"token_endpoint":                                 b + "/oauth/token",
		"registration_endpoint":                          b + "/oauth/register",
		"revocation_endpoint":                            b + "/oauth/revoke",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"revocation_endpoint_auth_methods_supported":     []string{"none"},
		"scopes_supported":                               apiTokenBusiness.AllScopes,
		"authorization_response_iss_parameter_supported": true,
	}
}

// ---------------------------------------------------------------------------
// Seams. The database and the agent layer are reached through these so the
// whole flow is tested without either.

var (
	createClient   = model.CreateClient
	getClient      = model.GetClient
	createRequest  = model.CreateRequest
	getRequest     = model.GetRequest
	takeRequest    = model.TakeRequest
	createCode     = model.CreateCode
	takeCode       = model.TakeCode
	createGrant    = model.CreateGrant
	grantByRefresh = model.GrantByRefresh
	grantByToken   = model.GrantByToken
	rotateRefresh  = model.RotateRefresh
	revokeGrant    = model.RevokeGrant
	liveGrantsFor  = model.LiveGrantsFor
	connectionsFor = model.ConnectionsFor
	grantOwner     = model.GrantOwner

	mintToken    = apiTokenBusiness.MintToken
	rotateSecret = apiTokenBusiness.RotateSecret
	revokeToken  = func(ctx context.Context, id uuid.UUID) error { _, err := tokenModel.RevokeAny(ctx, id); return err }
	tokenByHash  = tokenModel.GetActiveByHash

	listAgents  = agentBusiness.ListAgents
	createAgent = agentBusiness.CreateAgent
	getAgent    = agentModel.GetAgentByID
	userIsAdmin = func(ctx context.Context, id uuid.UUID) (bool, bool) {
		u, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, id)
		if err != nil || u == nil || u.Id == uuid.Nil {
			return false, false
		}
		return u.IsAdmin, true
	}
	surfaceEnabled = func(ctx context.Context) bool {
		s, _, err := mcpBusiness.Admission(ctx)
		return err == nil && s.Enabled && strings.TrimSpace(s.ToolGroups) != ""
	}
	now = time.Now
)

// ---------------------------------------------------------------------------
// Secrets

func randomSecret(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// verifyPKCE checks an S256 code verifier against the challenge the client
// sent when it started. RFC 7636: 43 to 128 characters from the unreserved set.
func verifyPKCE(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, r := range verifier {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r)) {
			return false
		}
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// ---------------------------------------------------------------------------
// Redirect addresses

func isLoopbackHost(host string) bool {
	h := strings.Trim(host, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// validRedirectURI accepts https anywhere, http only on a loopback address
// (a native client such as Claude Code, RFC 8252), and a private-use scheme
// for desktop apps (cursor://, vscode://). Never a fragment, and never a scheme
// a browser would execute.
func validRedirectURI(raw string) bool {
	if raw == "" || len(raw) > maxURILen {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Scheme == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return u.Host != ""
	case "http":
		return isLoopbackHost(u.Hostname())
	case "javascript", "data", "file", "vbscript", "about", "blob":
		return false
	default:
		return true
	}
}

// redirectMatches compares a requested redirect address with a registered
// one: exactly, except that a loopback address may use any port, because a
// native client picks a free one each time (RFC 8252 section 7.3).
func redirectMatches(registered, requested string) bool {
	if registered == requested {
		return true
	}
	a, errA := url.Parse(registered)
	b, errB := url.Parse(requested)
	if errA != nil || errB != nil || a.Scheme != "http" || b.Scheme != "http" {
		return false
	}
	if !isLoopbackHost(a.Hostname()) || !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	return a.Path == b.Path && a.RawQuery == b.RawQuery
}

func withQuery(base string, params url.Values) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// ---------------------------------------------------------------------------
// Scopes

// parseScopes keeps the grantable scopes a client asked for and drops the rest
// (offline_access, openid and the like are not ours to grant or refuse). Asking
// for nothing we know means asking for everything; the person narrows it on
// the consent screen.
func parseScopes(raw string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range strings.Fields(raw) {
		if apiTokenBusiness.ValidScopes([]string{s}) && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		out = append([]string(nil), apiTokenBusiness.AllScopes...)
	}
	return out
}

// ---------------------------------------------------------------------------
// Registration (RFC 7591)

// RegisterInput is the part of a registration request we read.
type RegisterInput struct {
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
}

// Register records a public client and answers with its client_id.
func Register(ctx context.Context, in RegisterInput) (map[string]interface{}, *Error) {
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > maxRedirectURIs {
		return nil, oauthErr("invalid_redirect_uri", fmt.Sprintf("give between 1 and %d redirect_uris", maxRedirectURIs))
	}
	for _, u := range in.RedirectURIs {
		if !validRedirectURI(u) {
			return nil, oauthErr("invalid_redirect_uri", "redirect_uris must be https, a loopback http address, or an app scheme: "+u)
		}
	}
	name := strings.TrimSpace(in.ClientName)
	if name == "" {
		name = "MCP client"
	}
	if r := []rune(name); len(r) > maxNameLen {
		name = string(r[:maxNameLen])
	}
	id, err := createClient(ctx, name, in.RedirectURIs)
	if err != nil {
		return nil, &Error{Code: "server_error", Description: "could not register the client", Status: 500}
	}
	return map[string]interface{}{
		"client_id":                  id.String(),
		"client_id_issued_at":        now().Unix(),
		"client_name":                name,
		"redirect_uris":              in.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}, nil
}

// ---------------------------------------------------------------------------
// Authorization

// AuthorizeResult is where to send the browser next. When Local is true the
// target is this workspace's own page (consent, or an error the client cannot
// be told about because its redirect address is not trusted).
type AuthorizeResult struct {
	Redirect string
	Local    bool
}

// consentURL is the workspace page a person approves a sign-in on.
func consentURL(q url.Values) string {
	return authService.FrontendBaseURL() + "/connect/authorize?" + q.Encode()
}

func localError(code string) AuthorizeResult {
	return AuthorizeResult{Redirect: consentURL(url.Values{"error": {code}}), Local: true}
}

// Authorize validates a sign-in request and parks it for the person's approval.
//
// Until the client and its redirect address are established, errors go to our
// own page: sending them to an unverified address is how an authorization
// endpoint becomes an open redirect. After that, they go back to the client as
// RFC 6749 says.
func Authorize(ctx context.Context, q url.Values) AuthorizeResult {
	clientID, err := uuid.Parse(q.Get("client_id"))
	if err != nil {
		return localError("invalid_client")
	}
	client, err := getClient(ctx, clientID)
	if err != nil {
		return localError("server_error")
	}
	if client == nil {
		return localError("invalid_client")
	}

	redirect := q.Get("redirect_uri")
	if redirect == "" && len(client.RedirectURIs) == 1 {
		redirect = client.RedirectURIs[0]
	}
	known := false
	for _, r := range client.RedirectURIs {
		if redirectMatches(r, redirect) {
			known = true
			break
		}
	}
	if !known {
		return localError("invalid_redirect_uri")
	}

	state := q.Get("state")
	back := func(code, desc string) AuthorizeResult {
		v := url.Values{"error": {code}, "iss": {BaseURL()}}
		if desc != "" {
			v.Set("error_description", desc)
		}
		if state != "" {
			v.Set("state", state)
		}
		return AuthorizeResult{Redirect: withQuery(redirect, v)}
	}
	if len(state) > maxStateLen {
		return back("invalid_request", "state is too long")
	}
	if q.Get("response_type") != "code" {
		return back("unsupported_response_type", "only the authorization code flow is supported")
	}
	challenge := q.Get("code_challenge")
	if challenge == "" || q.Get("code_challenge_method") != "S256" {
		return back("invalid_request", "PKCE with code_challenge_method=S256 is required")
	}
	if res := strings.TrimRight(q.Get("resource"), "/"); res != "" && res != ResourceURL() && res != BaseURL() {
		return back("invalid_target", "this server grants access to "+ResourceURL())
	}

	id, err := createRequest(ctx, &model.Request{
		ClientID:      client.ID,
		RedirectURI:   redirect,
		State:         state,
		Scopes:        parseScopes(q.Get("scope")),
		CodeChallenge: challenge,
		ExpiresAt:     now().Add(requestTTL),
	})
	if err != nil {
		return back("server_error", "")
	}
	return AuthorizeResult{Redirect: consentURL(url.Values{"request": {id.String()}}), Local: true}
}

// ---------------------------------------------------------------------------
// Consent

// ConsentAgent is an agent the person may connect the client as.
type ConsentAgent struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Tools int       `json:"tools"`
}

// ConsentView is what the approval page shows.
type ConsentView struct {
	ClientName string `json:"client_name"`
	// RedirectHost is where the approval is sent. Shown because the client
	// names itself, and the address is the part that cannot be faked.
	RedirectHost string         `json:"redirect_host"`
	Scopes       []string       `json:"scopes"`
	Agents       []ConsentAgent `json:"agents"`
	// SuggestedAgentID is an agent already made for this client, so a
	// reconnect reuses it rather than making a second.
	SuggestedAgentID *uuid.UUID `json:"suggested_agent_id,omitempty"`
	SurfaceEnabled   bool       `json:"surface_enabled"`
	IsAdmin          bool       `json:"is_admin"`
	ExpiresAt        time.Time  `json:"expires_at"`
}

// ErrRequestGone is a sign-in that expired, was answered, or never existed.
var ErrRequestGone = errors.New("this sign-in has expired or was already answered; start it again from your agent")

// ErrSurfaceClosed is the MCP surface being off (or exposing nothing).
var ErrSurfaceClosed = errors.New("an admin has not allowed outside agents in this workspace yet")

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.Host == "" {
		return u.Scheme + ":"
	}
	return u.Host
}

// Consent describes a pending sign-in to the person who is signed in.
func Consent(ctx context.Context, requestID uuid.UUID, actor agentBusiness.Actor) (*ConsentView, error) {
	req, err := getRequest(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, ErrRequestGone
	}
	client, err := getClient(ctx, req.ClientID)
	if err != nil || client == nil {
		return nil, ErrRequestGone
	}
	agents, err := listAgents(ctx, actor)
	if err != nil {
		return nil, err
	}
	view := &ConsentView{
		ClientName:     client.Name,
		RedirectHost:   hostOf(req.RedirectURI),
		Scopes:         req.Scopes,
		Agents:         []ConsentAgent{},
		SurfaceEnabled: surfaceEnabled(ctx),
		IsAdmin:        actor.IsAdmin,
		ExpiresAt:      req.ExpiresAt,
	}
	for _, a := range agents {
		// Only agents that can hold a credential, and only the person's own: an
		// admin connecting a client as a colleague's agent would lend that
		// colleague's name to a key they never approved.
		if !a.IsActive || a.CreatedBy != actor.UserID {
			continue
		}
		view.Agents = append(view.Agents, ConsentAgent{ID: a.Id, Name: a.Name, Tools: len(a.EnabledToolList())})
		if view.SuggestedAgentID == nil && strings.EqualFold(a.Name, client.Name) {
			id := a.Id
			view.SuggestedAgentID = &id
		}
	}
	sort.SliceStable(view.Agents, func(i, j int) bool { return view.Agents[i].Name < view.Agents[j].Name })
	return view, nil
}

// ApproveInput is the person's answer.
type ApproveInput struct {
	// AgentID connects the client as an agent the person already has. Empty
	// makes a new agent named after the client.
	AgentID string   `json:"agent_id"`
	Scopes  []string `json:"scopes"`
}

// ApproveResult is where to send the browser, and the agent the client acts as.
type ApproveResult struct {
	Redirect  string    `json:"redirect"`
	AgentID   uuid.UUID `json:"agent_id"`
	AgentName string    `json:"agent_name"`
	Created   bool      `json:"created"`
}

// Approve answers a pending sign-in with yes.
func Approve(ctx context.Context, requestID uuid.UUID, actor agentBusiness.Actor, in ApproveInput) (*ApproveResult, error) {
	if !surfaceEnabled(ctx) {
		return nil, ErrSurfaceClosed
	}
	req, err := getRequest(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, ErrRequestGone
	}
	client, err := getClient(ctx, req.ClientID)
	if err != nil || client == nil {
		return nil, ErrRequestGone
	}

	// Only what the client asked for, and at least one of it.
	asked := map[string]bool{}
	for _, s := range req.Scopes {
		asked[s] = true
	}
	var scopes []string
	for _, s := range in.Scopes {
		if asked[s] {
			scopes = append(scopes, s)
			delete(asked, s)
		}
	}
	if len(scopes) == 0 {
		return nil, errors.New("choose at least one thing it may do")
	}

	// A choice that cannot work is refused while the request is still open, so
	// the person can pick again without restarting from their client.
	if err := checkAgentChoice(ctx, actor, in.AgentID); err != nil {
		return nil, err
	}
	// Answered once: whoever takes it first wins, and a second approval (a
	// double click) finds nothing, so it cannot make a second agent either.
	if req, err = takeRequest(ctx, requestID); err != nil || req == nil {
		return nil, ErrRequestGone
	}
	agent, created, err := agentForConnection(ctx, actor, client.Name, in.AgentID, scopes)
	if err != nil {
		return nil, err
	}
	code, err := randomSecret("")
	if err != nil {
		return nil, err
	}
	if err := createCode(ctx, &model.Code{
		Hash:          hashSecret(code),
		ClientID:      client.ID,
		UserID:        actor.UserID,
		AgentID:       agent.Id,
		Scopes:        scopes,
		RedirectURI:   req.RedirectURI,
		CodeChallenge: req.CodeChallenge,
		ExpiresAt:     now().Add(codeTTL),
	}); err != nil {
		return nil, err
	}
	v := url.Values{"code": {code}, "iss": {BaseURL()}}
	if req.State != "" {
		v.Set("state", req.State)
	}
	return &ApproveResult{
		Redirect:  withQuery(req.RedirectURI, v),
		AgentID:   agent.Id,
		AgentName: agent.Name,
		Created:   created,
	}, nil
}

// agentForConnection is the identity a client connects as: one the person
// already has and chose, or a new one named after the client with exactly the
// tools the granted scopes reach.
func agentForConnection(ctx context.Context, actor agentBusiness.Actor, clientName, agentID string, scopes []string) (*agentModel.AiAgent, bool, error) {
	if agentID != "" {
		a, err := chosenAgent(ctx, actor, agentID)
		return a, false, err
	}
	a, err := createAgent(ctx, agentBusiness.AgentInput{
		Name:         clientName,
		Description:  "Connected from " + clientName + " over MCP. It acts with your permissions, only through the tools below.",
		EnabledTools: apiTokenBusiness.ToolsForScopes(scopes),
		IsActive:     true,
	}, actor.UserID)
	if err != nil {
		return nil, false, err
	}
	return a, true, nil
}

// chosenAgent is an existing agent the person picked, if they may connect as it:
// their own, live and not paused.
func chosenAgent(ctx context.Context, actor agentBusiness.Actor, agentID string) (*agentModel.AiAgent, error) {
	id, err := uuid.Parse(agentID)
	if err != nil {
		return nil, errors.New("invalid agent")
	}
	a, err := getAgent(ctx, id)
	if err != nil || a == nil || a.DeletedAt != nil {
		return nil, errors.New("that agent no longer exists")
	}
	if a.CreatedBy != actor.UserID {
		return nil, errors.New("you can only connect a client as an agent you sponsor")
	}
	if !a.IsActive {
		return nil, fmt.Errorf("%s is paused; resume it before connecting", a.Name)
	}
	return a, nil
}

func checkAgentChoice(ctx context.Context, actor agentBusiness.Actor, agentID string) error {
	if agentID == "" {
		return nil
	}
	_, err := chosenAgent(ctx, actor, agentID)
	return err
}

// Deny answers a pending sign-in with no.
func Deny(ctx context.Context, requestID uuid.UUID) (string, error) {
	req, err := takeRequest(ctx, requestID)
	if err != nil {
		return "", err
	}
	if req == nil {
		return "", ErrRequestGone
	}
	v := url.Values{"error": {"access_denied"}, "iss": {BaseURL()}}
	if req.State != "" {
		v.Set("state", req.State)
	}
	return withQuery(req.RedirectURI, v), nil
}

// ---------------------------------------------------------------------------
// Tokens

// TokenResponse is RFC 6749 section 5.1.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope,omitempty"`
}

var errInvalidGrant = oauthErr("invalid_grant", "the code or refresh token is invalid, expired or already used")

// Token serves the token endpoint.
func Token(ctx context.Context, form url.Values) (*TokenResponse, *Error) {
	switch form.Get("grant_type") {
	case "authorization_code":
		return exchangeCode(ctx, form)
	case "refresh_token":
		return refresh(ctx, form)
	default:
		return nil, oauthErr("unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

func exchangeCode(ctx context.Context, form url.Values) (*TokenResponse, *Error) {
	code, err := takeCode(ctx, hashSecret(form.Get("code")))
	if err != nil {
		return nil, &Error{Code: "server_error", Status: 500}
	}
	if code == nil {
		return nil, errInvalidGrant
	}
	if cid := form.Get("client_id"); cid != "" && cid != code.ClientID.String() {
		return nil, errInvalidGrant
	}
	if r := form.Get("redirect_uri"); r != "" && r != code.RedirectURI {
		return nil, errInvalidGrant
	}
	if !verifyPKCE(form.Get("code_verifier"), code.CodeChallenge) {
		return nil, oauthErr("invalid_grant", "code_verifier does not match the code_challenge")
	}
	client, err := getClient(ctx, code.ClientID)
	if err != nil || client == nil {
		return nil, errInvalidGrant
	}
	isAdmin, active := userIsAdmin(ctx, code.UserID)
	if !active {
		return nil, oauthErr("invalid_grant", "the person who approved this is no longer active")
	}

	// A reconnect replaces the earlier connection for the same client and agent,
	// so repeated sign-ins do not leave keys nobody uses.
	if old, err := liveGrantsFor(ctx, code.UserID, code.AgentID, client.Name); err == nil {
		for _, g := range old {
			endGrant(ctx, g)
		}
	}

	expires := now().Add(AccessTTL)
	agentID := code.AgentID
	minted, err := mintToken(ctx, client.Name+" (connected)", code.Scopes, &expires, code.UserID, &agentID, isAdmin)
	if err != nil {
		return nil, oauthErr("invalid_grant", err.Error())
	}
	refreshToken, err := randomSecret(refreshPrefix)
	if err != nil {
		return nil, &Error{Code: "server_error", Status: 500}
	}
	if err := createGrant(ctx, &model.Grant{
		ClientID:         client.ID,
		UserID:           code.UserID,
		AgentID:          code.AgentID,
		TokenID:          minted.Token.Id,
		RefreshExpiresAt: now().Add(RefreshTTL),
	}, hashSecret(refreshToken)); err != nil {
		_ = revokeToken(ctx, minted.Token.Id)
		return nil, &Error{Code: "server_error", Status: 500}
	}
	return &TokenResponse{
		AccessToken:  minted.Plaintext,
		TokenType:    "Bearer",
		ExpiresIn:    int(AccessTTL.Seconds()),
		RefreshToken: refreshToken,
		Scope:        strings.Join(code.Scopes, " "),
	}, nil
}

func refresh(ctx context.Context, form url.Values) (*TokenResponse, *Error) {
	oldHash := hashSecret(form.Get("refresh_token"))
	g, err := grantByRefresh(ctx, oldHash)
	if err != nil {
		return nil, &Error{Code: "server_error", Status: 500}
	}
	if g == nil {
		return nil, errInvalidGrant
	}
	if cid := form.Get("client_id"); cid != "" && cid != g.ClientID.String() {
		return nil, errInvalidGrant
	}
	next, err := randomSecret(refreshPrefix)
	if err != nil {
		return nil, &Error{Code: "server_error", Status: 500}
	}
	ok, err := rotateRefresh(ctx, g.ID, oldHash, hashSecret(next), now().Add(RefreshTTL))
	if err != nil {
		return nil, &Error{Code: "server_error", Status: 500}
	}
	if !ok {
		return nil, errInvalidGrant
	}
	access, err := rotateSecret(ctx, g.TokenID, now().Add(AccessTTL))
	if err != nil {
		// The credential was revoked (from the inventory, or by its owner), so
		// the connection is over; end the grant so the refresh token dies too.
		endGrant(ctx, g.ID)
		return nil, oauthErr("invalid_grant", "this connection was revoked; connect again")
	}
	return &TokenResponse{
		AccessToken:  access,
		TokenType:    "Bearer",
		ExpiresIn:    int(AccessTTL.Seconds()),
		RefreshToken: next,
	}, nil
}

func endGrant(ctx context.Context, grantID uuid.UUID) {
	if tokenID, err := revokeGrant(ctx, grantID); err == nil && tokenID != nil {
		_ = revokeToken(ctx, *tokenID)
	}
}

// Revoke ends the connection a token belongs to, whichever of its two tokens
// is presented (RFC 7009). Unknown tokens are not an error: the answer is the
// same either way, so it discloses nothing.
func Revoke(ctx context.Context, token string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	if strings.HasPrefix(token, refreshPrefix) {
		if g, err := grantByRefresh(ctx, hashSecret(token)); err == nil && g != nil {
			endGrant(ctx, g.ID)
		}
		return
	}
	t, err := tokenByHash(ctx, apiTokenBusiness.HashToken(token))
	if err != nil || t == nil {
		return
	}
	if g, err := grantByToken(ctx, t.Id); err == nil && g != nil {
		endGrant(ctx, g.ID)
		return
	}
	_ = revokeToken(ctx, t.Id)
}

// ---------------------------------------------------------------------------
// A person's own connections

// MyConnectionsView is what a member sees about outside agents: whether the
// workspace lets them in at all, and the ones this person has connected.
type MyConnectionsView struct {
	Open        bool               `json:"open"`
	Connections []model.Connection `json:"connections"`
}

// MyConnections lists the caller's live connections. Members see only their
// own; the admin inventory is where every agent in the workspace is listed.
func MyConnections(ctx context.Context, userID uuid.UUID) (*MyConnectionsView, error) {
	conns, err := connectionsFor(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &MyConnectionsView{Open: surfaceEnabled(ctx), Connections: conns}, nil
}

// ErrNotYourConnection covers a connection that is someone else's and one that
// has already ended alike, so the answer discloses nothing about other people.
var ErrNotYourConnection = errors.New("that connection is not yours, or it has already ended")

// Disconnect ends one of the caller's connections: the grant stops refreshing
// and its access token stops working at once. The agent it acted as is kept,
// with its history, so the audit still reads.
func Disconnect(ctx context.Context, userID, grantID uuid.UUID) error {
	owner, err := grantOwner(ctx, grantID)
	if err != nil {
		return err
	}
	if owner == uuid.Nil || owner != userID {
		return ErrNotYourConnection
	}
	tokenID, err := revokeGrant(ctx, grantID)
	if err != nil {
		return err
	}
	if tokenID != nil {
		return revokeToken(ctx, *tokenID)
	}
	return nil
}
