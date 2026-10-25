package business

// Connecting a provider by signing in at it (provider.OAuthConnector), for
// one with no personal tokens to paste (Basecamp). StartOAuthConnect mints a
// one-shot state bound to the admin and returns the provider's authorize
// page; the provider sends the admin back to the server's callback, and
// FinishOAuthConnect trades the code for a token and saves it as a pasted
// token is saved (SaveConnection): tested first, then kept.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

type oauthState struct {
	Provider string    `json:"p"`
	Owner    uuid.UUID `json:"o"`
}

// SignInConnector is the provider's sign-in when it offers one, nil when it
// is connected with a token. Its Configured says whether this server has the
// provider's app.
func SignInConnector(prov importProvider.Provider) *importProvider.OAuthConnect {
	c, ok := prov.(importProvider.OAuthConnector)
	if !ok {
		return nil
	}
	conn := c.OAuthConnect()
	return &conn
}

// StartOAuthConnect is the authorize page that begins owner's sign-in at
// prov, coming back to redirectURI.
func StartOAuthConnect(ctx context.Context, prov importProvider.Provider, owner uuid.UUID, redirectURI string) (string, *ProviderProblem) {
	conn := SignInConnector(prov)
	name := providerName(prov.Name())
	if conn == nil {
		return "", &ProviderProblem{Status: http.StatusBadRequest, Code: "no_sign_in", Msg: name + " is connected with a token, not by signing in."}
	}
	if !conn.Configured() {
		return "", &ProviderProblem{Status: http.StatusServiceUnavailable, Code: "not_configured", Msg: conn.Setup}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", &ProviderProblem{Status: http.StatusServiceUnavailable, Code: "provider_error", Msg: "Couldn't start signing in to " + name + ". Try again."}
	}
	state := hex.EncodeToString(buf)
	val, _ := json.Marshal(oauthState{Provider: prov.Name(), Owner: owner})
	if err := redisStore.SetString(ctx, registry.ImportOAuthState, []string{state}, string(val)); err != nil {
		return "", &ProviderProblem{Status: http.StatusServiceUnavailable, Code: "provider_error", Msg: "Couldn't start signing in to " + name + ". Try again."}
	}
	return conn.AuthURL(state, redirectURI), nil
}

// FinishOAuthConnect completes the sign-in state began for owner: the code
// becomes a token, which is tested and saved. It answers the provider's
// name, for the page the admin goes back to.
func FinishOAuthConnect(ctx context.Context, state, code string, owner uuid.UUID, redirectURI string) (string, *ProviderProblem) {
	expired := &ProviderProblem{Status: http.StatusBadRequest, Code: "sign_in_expired", Msg: "That sign-in has expired. Connect again."}
	if state == "" || len(state) > 128 || code == "" {
		return "", expired
	}
	raw, hit, err := redisStore.GetDelString(ctx, registry.ImportOAuthState, []string{state})
	if err != nil || !hit {
		return "", expired
	}
	var st oauthState
	// A state minted for another admin is never theirs to finish.
	if json.Unmarshal([]byte(raw), &st) != nil || st.Owner != owner {
		return "", expired
	}
	prov := importProvider.Get(st.Provider)
	var conn *importProvider.OAuthConnect
	if prov != nil {
		conn = SignInConnector(prov)
	}
	if conn == nil || !conn.Configured() {
		return st.Provider, expired
	}
	t, err := conn.Exchange(ctx, code, redirectURI)
	if err != nil {
		return st.Provider, &ProviderProblem{Status: http.StatusBadRequest, Code: "token_rejected",
			Msg: providerName(st.Provider) + " didn't accept the sign-in. Connect again."}
	}
	tok := &importModels.Token{Provider: st.Provider, OwnerUserId: owner, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken}
	if !t.Expiry.IsZero() {
		e := t.Expiry
		tok.ExpiresAt = &e
	}
	if _, problem := SaveConnection(ctx, prov, tok, ""); problem != nil {
		return st.Provider, problem
	}
	return st.Provider, nil
}

// SaveConnection asks the provider what tok can see, then saves it and
// keeps that list as the discovery answer. A token the provider refuses is
// said, and not saved. For a pasted token and a sign-in alike. A site the
// admin typed is held to the outbound rules first (importProvider.CheckSite),
// and one they refuse is never tried.
func SaveConnection(ctx context.Context, prov importProvider.Provider, tok *importModels.Token, site string) ([]importProvider.DiscoverItem, *ProviderProblem) {
	if site != "" {
		typed := site
		if !strings.Contains(typed, "://") {
			typed = "https://" + typed
		}
		if err := importProvider.CheckSite(typed); err != nil {
			p := DescribeProviderError(prov.Name(), site, err)
			return nil, &p
		}
	}
	items, problem := TestConnection(ctx, prov, tok.OwnerUserId.String(), tok, site)
	if problem != nil {
		return nil, problem
	}
	if err := importModels.SaveToken(ctx, tok); err != nil {
		return nil, &ProviderProblem{Status: http.StatusInternalServerError, Code: "provider_error", Msg: "Couldn't save the connection. Try again."}
	}
	key := []string{tok.Provider, tok.OwnerUserId.String()}
	// A new token may see another account: the old list goes.
	_ = redisStore.Delete(ctx, registry.ImportDiscover, key)
	if items != nil {
		_ = redisStore.SetJSON(ctx, registry.ImportDiscover, key, items)
	}
	importProvider.InvalidateImportTokenSource(tok.Provider, tok.OwnerUserId)
	return items, nil
}
