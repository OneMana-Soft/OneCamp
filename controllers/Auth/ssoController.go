package controllers

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	business "github.com/akashc777/OneCamp/business/User"
	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/oauth"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	authService "github.com/akashc777/OneCamp/services/Auth"
	ldapService "github.com/akashc777/OneCamp/services/LDAP"
	samlService "github.com/akashc777/OneCamp/services/SAML"
	oidc "github.com/coreos/go-oidc/v3/oidc"
	saml "github.com/crewjam/saml"
	"golang.org/x/oauth2"
)

// emailRegex is defined in authController.go and shared across this package.

// ----------------------------------------------------------------------------
// LDAP
// ----------------------------------------------------------------------------

// LDAPLogin handles POST /auth/ldap-login.
//
// Flow:
//  1. Parse body, validate required fields.
//  2. Resolve LDAP config (single-tenant today; per-org tomorrow).
//  3. Bind + search + re-bind via the LDAP service.
//  4. JIT-provision if first time, marking the user as SSO-managed.
//  5. Issue auth cookies via the canonical helper from authController.go.
func LDAPLogin(w http.ResponseWriter, r *http.Request) {
	if planLocked(w, r, helpers.FeatureLDAP, false) {
		return
	}
	ctx := r.Context()

	cfg := authService.ResolveLDAPConfig(ctx)
	if !cfg.Enabled {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg":    "LDAP authentication is disabled",
			"status": "failed",
		})
		return
	}

	var requestBody struct {
		UsernameOrEmail string `json:"username_or_email"`
		Password        string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid request body",
			"status": "failed",
		})
		return
	}

	requestBody.UsernameOrEmail = strings.TrimSpace(requestBody.UsernameOrEmail)
	if requestBody.UsernameOrEmail == "" || requestBody.Password == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "username/email and password are required",
			"status": "failed",
		})
		return
	}

	if cfg.Host == "" || cfg.BaseDN == "" || cfg.UserFilter == "" {
		helpers.LogErrorWithContext(ctx,
			"controllers/LDAPLogin missing config: host=%q baseDN=%q filter=%q",
			cfg.Host, cfg.BaseDN, cfg.UserFilter)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{
			"msg":    "Directory authentication is not fully configured",
			"status": "failed",
		})
		return
	}

	client := &ldapService.LDAPClient{
		Host:           cfg.Host,
		Port:           cfg.Port,
		UseTLS:         cfg.UseTLS,
		BindDN:         cfg.BindDN,
		BindPassword:   cfg.BindPassword,
		BaseDN:         cfg.BaseDN,
		UserFilter:     cfg.UserFilter,
		GroupAttribute: cfg.GroupAttribute,
	}

	ldapUser, err := client.Authenticate(requestBody.UsernameOrEmail, requestBody.Password)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/LDAPLogin auth failed: %+v", err)
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
			"msg":    "Invalid directory credentials",
			"status": "failed",
		})
		return
	}

	// SECURITY: refuse to provision a user without an authoritative email.
	// The previous "<username>@ldap.local" fallback collides across orgs that
	// share usernames and would log the second user into the first user's
	// account.
	if ldapUser.Email == "" || !strings.Contains(ldapUser.Email, "@") {
		helpers.LogErrorWithContext(ctx,
			"controllers/LDAPLogin user %q missing email; refusing to provision", ldapUser.DN)
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
			"msg":    "Your directory account has no email attribute. Contact your administrator.",
			"status": "failed",
		})
		return
	}

	email := strings.ToLower(strings.TrimSpace(ldapUser.Email))
	if !emailRegex.MatchString(email) {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
			"msg":    "Your directory account has an invalid email format. Contact your administrator.",
			"status": "failed",
		})
		return
	}

	username := strings.TrimSpace(ldapUser.Username)
	if username == "" {
		username = strings.Split(email, "@")[0]
	}

	user, err := lookupOrProvision(ctx, email, username, userModels.AuthMethodLDAP)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/LDAPLogin lookupOrProvision: %+v", err)
		if helpers.WriteSeatLimit(w, err) {
			return
		}
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "Failed to provision account",
			"status": "failed",
		})
		return
	}

	_ = domain.RecordLoginMethod(ctx, user.Id, userModels.AuthMethodLDAP)

	// Best-effort admin sync from directory groups.
	syncSSOAdmin(ctx, email, user.IsSSOManaged, user.IsAdmin, ldapUser.Groups, cfg.AdminGroupAllow)

	// issueAuthCookies (in authController.go) writes the success JSON envelope
	// and Set-Cookie headers.
	issueAuthCookies(w, r, ctx, user.Id.String())
}

// ----------------------------------------------------------------------------
// SAML
// ----------------------------------------------------------------------------

// SAMLLogin handles GET /saml/login by delegating to the crewjam SAML
// middleware, which manages request tracking (the InResponseTo nonce) so the
// callback can validate replies properly.
func SAMLLogin(w http.ResponseWriter, r *http.Request) {
	if planLocked(w, r, helpers.FeatureSSO, true) {
		return
	}
	if !authService.ResolveSAMLConfig(r.Context()).Enabled || samlService.SAMLMiddleware == nil {
		http.Error(w, "SAML is disabled", http.StatusForbidden)
		return
	}

	// RequireAccount kicks the browser to the IdP if there's no SAML session,
	// minting and tracking a request ID along the way. The handler we hand it
	// is unreachable (we always 302 to the IdP first) but must be non-nil.
	dummy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	samlService.SAMLMiddleware.RequireAccount(dummy).ServeHTTP(w, r)
}

// SAMLMetadata exposes the SP metadata for IdP configuration.
func SAMLMetadata(w http.ResponseWriter, r *http.Request) {
	if planLocked(w, r, helpers.FeatureSSO, false) {
		return
	}
	if !authService.ResolveSAMLConfig(r.Context()).Enabled || samlService.SAMLMiddleware == nil {
		http.Error(w, "SAML is disabled", http.StatusForbidden)
		return
	}

	descriptor := samlService.SAMLMiddleware.ServiceProvider.Metadata()
	buf, err := xml.MarshalIndent(descriptor, "", "  ")
	if err != nil {
		http.Error(w, "failed to marshal SAML metadata", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

// SAMLCallback handles POST /saml/acs.
//
// Replaces the earlier hand-rolled ParseResponse([]string{""}) which only
// accepted IdP-initiated assertions. We now consult the middleware's tracked
// requests so SP-initiated and IdP-initiated both validate correctly.
//
// We additionally refuse assertions whose Subject isn't an email-format
// NameID and whose AttributeStatements don't yield a valid email.
func SAMLCallback(w http.ResponseWriter, r *http.Request) {
	if planLocked(w, r, helpers.FeatureSSO, true) {
		return
	}
	ctx := r.Context()

	if !authService.ResolveSAMLConfig(ctx).Enabled || samlService.SAMLMiddleware == nil {
		http.Redirect(w, r, ssoErrorRedirect("saml_disabled", "SAML is disabled"), http.StatusFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SAMLCallback form parse err: %+v", err)
		http.Redirect(w, r, ssoErrorRedirect("saml_invalid", "Malformed SAML response"), http.StatusFound)
		return
	}

	// Build the set of acceptable in-flight request IDs from the tracker.
	// Empty string is included to permit IdP-initiated SSO flows.
	possibleRequestIDs := []string{""}
	if samlService.SAMLMiddleware.RequestTracker != nil {
		for _, tr := range samlService.SAMLMiddleware.RequestTracker.GetTrackedRequests(r) {
			possibleRequestIDs = append(possibleRequestIDs, tr.SAMLRequestID)
		}
	}

	assertion, err := samlService.SAMLMiddleware.ServiceProvider.ParseResponse(r, possibleRequestIDs)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SAMLCallback ParseResponse err: %+v", err)
		http.Redirect(w, r, ssoErrorRedirect("saml_invalid", "Invalid SAML assertion from directory provider"), http.StatusFound)
		return
	}

	email, username, samlGroups, identityErr := extractSAMLIdentity(assertion)
	if identityErr != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SAMLCallback identity extract err: %+v", identityErr)
		http.Redirect(w, r, ssoErrorRedirect("saml_no_email", identityErr.Error()), http.StatusFound)
		return
	}

	target := authService.FrontendBaseURL() + "/app"
	cfg := authService.ResolveSAMLConfig(ctx)
	provisionAndRedirectWithGroups(w, r, ctx, email, username, userModels.AuthMethodSAML, target, samlGroups, cfg.AdminGroupAllow)
}

// extractSAMLIdentity pulls a usable email, username, and group set out of
// the assertion. Refuses non-email NameIDs and consults common email
// attribute names. Group attribute name is configured via
// SAML_ADMIN_GROUP_ATTR; common values are "memberOf" / "groups" / "Role".
//
// Email-format NameIDs (Format ending in :emailAddress) are accepted directly.
// Otherwise we walk AttributeStatements for a recognized email attribute.
func extractSAMLIdentity(assertion *saml.Assertion) (email, username string, groups []string, err error) {
	if assertion == nil {
		return "", "", nil, fmt.Errorf("nil SAML assertion")
	}

	// 1) Try NameID, but only if its Format declares email or the value
	// already looks like an email (some IdPs send NameID-Format=unspecified
	// with an email value; that's still safer than transient/persistent IDs).
	if assertion.Subject != nil && assertion.Subject.NameID != nil {
		nid := assertion.Subject.NameID
		nidValue := strings.ToLower(strings.TrimSpace(nid.Value))
		if isEmailNameIDFormat(nid.Format) && emailRegex.MatchString(nidValue) {
			email = nidValue
		} else if nid.Format == "" && emailRegex.MatchString(nidValue) {
			// Format omitted entirely — accept if the value parses as an email.
			email = nidValue
		}
	}

	// 2) Walk AttributeStatements for explicit email + username + groups.
	emailAttrNames := map[string]struct{}{
		"email":        {},
		"emailaddress": {},
		"mail":         {},
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress": {},
		"urn:oid:0.9.2342.19200300.100.1.3":                                  {},
	}
	usernameAttrNames := map[string]struct{}{
		"username":           {},
		"samaccountname":     {},
		"uid":                {},
		"preferred_username": {},
		"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name": {},
		"urn:oid:0.9.2342.19200300.100.1.1":                          {},
	}

	// Group attribute candidates. SAML_ADMIN_GROUP_ATTR is added on top of
	// these defaults so most IdPs work out-of-the-box.
	cfg := authService.ResolveSAMLConfig(context.Background())
	groupAttrCandidates := map[string]struct{}{
		"memberof": {},
		"groups":   {},
		"role":     {},
		"roles":    {},
		"http://schemas.microsoft.com/ws/2008/06/identity/claims/role": {},
	}
	if attr := strings.ToLower(strings.TrimSpace(cfg.AdminGroupAttr)); attr != "" {
		groupAttrCandidates[attr] = struct{}{}
	}

	for _, stmt := range assertion.AttributeStatements {
		for _, attr := range stmt.Attributes {
			key := strings.ToLower(strings.TrimSpace(attr.Name))
			if email == "" {
				if _, ok := emailAttrNames[key]; ok && len(attr.Values) > 0 {
					candidate := strings.ToLower(strings.TrimSpace(attr.Values[0].Value))
					if emailRegex.MatchString(candidate) {
						email = candidate
					}
				}
			}
			if username == "" {
				if _, ok := usernameAttrNames[key]; ok && len(attr.Values) > 0 {
					username = strings.TrimSpace(attr.Values[0].Value)
				}
			}
			if _, ok := groupAttrCandidates[key]; ok {
				for _, v := range attr.Values {
					if val := strings.TrimSpace(v.Value); val != "" {
						groups = append(groups, val)
					}
				}
			}
		}
	}

	if email == "" {
		return "", "", nil, fmt.Errorf("SAML assertion did not include a valid email NameID or attribute")
	}

	if username == "" {
		username = strings.Split(email, "@")[0]
	}
	return email, username, groups, nil
}

// isEmailNameIDFormat checks for the SAML Core email-format NameID URN.
// Both the legacy 1.1 and 2.0 formats are accepted.
func isEmailNameIDFormat(format string) bool {
	switch strings.TrimSpace(format) {
	case "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress",
		"urn:oasis:names:tc:SAML:2.0:nameid-format:emailAddress":
		return true
	}
	return false
}

// ----------------------------------------------------------------------------
// Generic OIDC
// ----------------------------------------------------------------------------

// GenericOIDCLogin handles GET /oauth_login/oidc.
//
// Mints a random opaque state, persists (state -> redirect URL) in Redis with
// 10-minute TTL, and uses the state as the OAuth state param. The IdP echoes
// it back on /oauth_callback/oidc; we reject any callback whose state doesn't
// resolve to a stored entry. This is the standard OIDC anti-CSRF pattern and
// also closes the open-redirect hole the previous code had (where the IdP-
// echoed redirect URL was trusted blindly).
func GenericOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if planLocked(w, r, helpers.FeatureSSO, true) {
		return
	}
	ctx := r.Context()

	cfg := authService.ResolveOIDCConfig(ctx)
	if !cfg.Enabled {
		http.Error(w, "OIDC is disabled", http.StatusForbidden)
		return
	}

	if oauth.OIDCGeneric.Config == nil || oauth.OIDCGeneric.Verifier == nil {
		// Init failed at boot (issuer unreachable, missing env, etc.) but the
		// route is still mounted. Don't panic — return a clear error.
		helpers.LogErrorWithContext(ctx, "controllers/GenericOIDCLogin OIDC enabled but not initialized")
		http.Error(w, "OIDC is misconfigured on the server", http.StatusServiceUnavailable)
		return
	}

	// Sanitize the redirect URL: only accept FE-host-anchored targets.
	requested := strings.TrimSpace(r.URL.Query().Get("redirect_uri"))
	redirectURL := authService.FrontendBaseURL() + "/app"
	if requested != "" && authService.IsRedirectAllowed(requested) {
		redirectURL = requested
	}

	state, err := authService.GenerateAndStoreSSOState(ctx, redirectURL)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GenericOIDCLogin state mint err: %+v", err)
		http.Redirect(w, r, ssoErrorRedirect("oidc_state_mint_failed", "Could not start OIDC login"), http.StatusFound)
		return
	}

	authURL := oauth.OIDCGeneric.Config.AuthCodeURL(state, oauth2.AccessTypeOffline)
	http.Redirect(w, r, authURL, http.StatusTemporaryRedirect)
}

// GenericOIDCCallback handles GET /oauth_callback/oidc.
func GenericOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if planLocked(w, r, helpers.FeatureSSO, true) {
		return
	}
	ctx := r.Context()

	cfg := authService.ResolveOIDCConfig(ctx)
	if !cfg.Enabled {
		http.Redirect(w, r, ssoErrorRedirect("oidc_disabled", "OIDC is disabled"), http.StatusFound)
		return
	}
	if oauth.OIDCGeneric.Config == nil || oauth.OIDCGeneric.Verifier == nil {
		http.Redirect(w, r, ssoErrorRedirect("oidc_misconfigured", "OIDC is misconfigured on the server"), http.StatusFound)
		return
	}

	state := r.FormValue("state")
	oauthCode := r.FormValue("code")
	if state == "" || oauthCode == "" {
		http.Redirect(w, r, ssoErrorRedirect("oidc_invalid_request", "Missing state or code"), http.StatusFound)
		return
	}

	// Atomically consume the state — single-use, prevents replay/CSRF.
	redirectURL, err := authService.ConsumeSSOState(ctx, state)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GenericOIDCCallback consume state err: %+v", err)
		http.Redirect(w, r, ssoErrorRedirect("oidc_invalid_state", "Login session expired or was tampered with"), http.StatusFound)
		return
	}

	oauth2Token, err := oauth.OIDCGeneric.Config.Exchange(ctx, oauthCode)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GenericOIDCCallback Exchange err: %+v", err)
		http.Redirect(w, r, ssoErrorRedirect("oidc_invalid_code", "Authorization code exchange failed"), http.StatusFound)
		return
	}

	rawIDToken, ok := oauth2Token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		http.Redirect(w, r, ssoErrorRedirect("oidc_no_token", "Identity provider did not return ID token"), http.StatusFound)
		return
	}

	idToken, err := oauth.OIDCGeneric.Verifier.Verify(ctx, rawIDToken)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GenericOIDCCallback Verify err: %+v", err)
		http.Redirect(w, r, ssoErrorRedirect("oidc_verification_failed", "ID token signature verification failed"), http.StatusFound)
		return
	}

	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		PreferredName string `json:"preferred_username"`
		Name          string `json:"name"`
		// Groups is the standard OIDC group claim. Not all IdPs emit it; many
		// emit "roles" or a custom claim — operator overrides via
		// OIDC_ADMIN_GROUP_CLAIM (handled below).
		Groups []string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Redirect(w, r, ssoErrorRedirect("oidc_invalid_claims", "Failed to extract directory claims"), http.StatusFound)
		return
	}

	// Pull the configured custom-claim group set, if any. This is decoded
	// permissively: claim may be a string or []string.
	customGroups := extractOIDCGroupsFromClaim(idToken, cfg.AdminGroupClaim)
	if len(customGroups) > 0 {
		claims.Groups = append(claims.Groups, customGroups...)
	}

	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" || !emailRegex.MatchString(email) {
		http.Redirect(w, r, ssoErrorRedirect("oidc_no_email", "OIDC provider did not provide a valid email"), http.StatusFound)
		return
	}

	// SECURITY: enforce email_verified unless the operator explicitly turned
	// the check off via OIDC_REQUIRE_VERIFIED_EMAIL=false (some private IdPs
	// don't emit the claim).
	if cfg.RequireVerifiedEmail && !claims.EmailVerified {
		helpers.LogErrorWithContext(ctx, "controllers/GenericOIDCCallback rejecting unverified email %q", email)
		http.Redirect(w, r, ssoErrorRedirect("oidc_email_unverified", "Your IdP reports this email as unverified"), http.StatusFound)
		return
	}

	username := claims.PreferredName
	if username == "" {
		username = claims.Name
	}
	if username == "" {
		username = strings.Split(email, "@")[0]
	}

	// Capture the groups for post-provision admin sync.
	provisionAndRedirectWithGroups(w, r, ctx, email, username, userModels.AuthMethodOIDC, redirectURL, claims.Groups, cfg.AdminGroupAllow)
}

// extractOIDCGroupsFromClaim reads a custom group claim from the ID token in
// either string or []string form. Returns an empty slice if the claim is
// missing or unparsable. Operator configures the claim name via
// OIDC_ADMIN_GROUP_CLAIM.
func extractOIDCGroupsFromClaim(idToken *oidc.IDToken, claimName string) []string {
	if claimName == "" {
		return nil
	}
	var bag map[string]any
	if err := idToken.Claims(&bag); err != nil {
		return nil
	}
	raw, ok := bag[claimName]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ----------------------------------------------------------------------------
// Shared helpers
// ----------------------------------------------------------------------------

// provisionAndRedirectWithGroups is the redirect-flow helper for SAML/OIDC. It looks up or
// creates the user, records the login method, reconciles admin group membership, sets
// cookies, and 302s to target. On failure it 302s to the FE login page with an error.
//
// claimedGroups come from the IdP assertion; allowGroups is the operator-configured admin
// allowlist. Empty allowGroups disables admin sync entirely (admin assignment stays manual).
//
// There is deliberately no shorter no-groups wrapper. One existed, passing nil for both
// group arguments, and was called by nothing — both live entry points (SAML and OIDC) call
// this function directly. Its own doc noted that admin sync "needs the *WithGroups variant",
// which is the problem: a wrapper with the more inviting name that silently skips admin
// group reconciliation is a footgun in an authentication path. Callers pass nil explicitly
// if that is genuinely what they mean.
func provisionAndRedirectWithGroups(
	w http.ResponseWriter,
	r *http.Request,
	ctx context.Context,
	email, username, method, target string,
	claimedGroups []string,
	allowGroups []string,
) {
	if target == "" || !authService.IsRedirectAllowed(target) {
		target = authService.FrontendBaseURL() + "/app"
	}

	user, err := lookupOrProvision(ctx, email, username, method)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/provisionAndRedirect: %+v", err)
		var seat *helpers.SeatLimitError
		if errors.As(err, &seat) {
			http.Redirect(w, r, ssoErrorRedirect("seat_limit", seat.Error()), http.StatusFound)
			return
		}
		http.Redirect(w, r, ssoErrorRedirect("provision_failed", "Failed to provision account"), http.StatusFound)
		return
	}

	_ = domain.RecordLoginMethod(ctx, user.Id, method)

	// Admin sync — best-effort, never fails the login.
	syncSSOAdmin(ctx, email, user.IsSSOManaged, user.IsAdmin, claimedGroups, allowGroups)

	if err := emitAuthCookiesNoBody(w, r, ctx, user.Id.String()); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/provisionAndRedirect emit cookies: %+v", err)
		http.Redirect(w, r, ssoErrorRedirect("session_failed", "Failed to start session"), http.StatusFound)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// syncSSOAdmin promotes/demotes the user's admin flag based on IdP-asserted
// groups intersected with the operator-configured allowlist. Logging-only
// on failure.
func syncSSOAdmin(ctx context.Context, email string, isSSOManaged, currentlyAdmin bool, claimedGroups, allowGroups []string) {
	if len(allowGroups) == 0 {
		return
	}
	shouldBeAdmin := authService.MatchAdminGroup(allowGroups, claimedGroups)

	if shouldBeAdmin && !currentlyAdmin {
		if err := domain.CreateAdminUser(ctx, email); err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/syncSSOAdmin promote err: %+v", err)
		} else {
			helpers.MessageLogs.InfoLog.Printf("SSO admin promotion: email=%s groups=%v", email, claimedGroups)
		}
		return
	}

	// Only revoke admins whose accounts are SSO-managed. Otherwise we'd risk
	// stripping admin from a local user who happened to also have an SSO
	// session (cross-auth scenarios are real for break-glass admins).
	if !shouldBeAdmin && currentlyAdmin && isSSOManaged {
		if err := domain.HardDeleteAdminUserByEmailId(ctx, email); err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/syncSSOAdmin revoke err: %+v", err)
		} else {
			helpers.MessageLogs.InfoLog.Printf("SSO admin revocation: email=%s no longer in groups=%v", email, allowGroups)
		}
	}
}

// lookupOrProvision returns the existing user or creates a fresh SSO-managed
// one. SSO-method users get is_sso_managed=true so SetPassword/ChangePassword
// reject them — there's no local-password backdoor to a user the IdP owns.
func lookupOrProvision(ctx context.Context, email, username, method string) (*userModels.User, error) {
	err, exists := domain.CheckIfUserExistByEmail(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("user existence check failed: %w", err)
	}

	if !exists {
		isSSO := userModels.IsSSOMethod(method)
		// nil passwordHash → no local password.
		if _, err := business.CreateUserWithMethod(ctx, email, username, nil, method, isSSO); err != nil {
			return nil, fmt.Errorf("create user: %w", err)
		}
	}

	user, err := domain.GetUserByEmailId(ctx, &email)
	if err != nil || user == nil {
		return nil, fmt.Errorf("post-provision lookup failed: %w", err)
	}
	return user, nil
}

// ssoErrorRedirect builds a frontend URL with the standardized error/message
// query params. Allowed error codes are documented in the FE login page.
// planLocked refuses a sign-in method the plan leaves out (helpers/planFeatures.go).
// A browser flow lands back on the sign-in page with the reason, the same way a
// full seat plan does; an API caller gets the JSON 403.
func planLocked(w http.ResponseWriter, r *http.Request, f helpers.PlanFeature, browser bool) bool {
	if helpers.PlanAllows(f) {
		return false
	}
	if browser {
		http.Redirect(w, r, ssoErrorRedirect("plan_required", helpers.PlanRequiredMessage(f)), http.StatusFound)
	} else {
		helpers.WritePlanRequired(w, f)
	}
	return true
}

func ssoErrorRedirect(errCode, errMsg string) string {
	feBase := authService.FrontendBaseURL()
	if feBase == "" {
		return fmt.Sprintf("/?error=%s&message=%s", url.QueryEscape(errCode), url.QueryEscape(errMsg))
	}
	u, err := url.Parse(feBase)
	if err != nil || u.Host == "" {
		return fmt.Sprintf("/?error=%s&message=%s", url.QueryEscape(errCode), url.QueryEscape(errMsg))
	}
	q := u.Query()
	q.Set("error", errCode)
	q.Set("message", errMsg)
	u.RawQuery = q.Encode()
	return u.String()
}
