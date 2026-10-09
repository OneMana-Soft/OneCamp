package router

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The set of endpoints reachable WITHOUT authentication is pinned here.
//
// WHY A TEST AND NOT A REVIEW HABIT
// ---------------------------------
// Authorisation in this router is positional. A sub-router inherits middleware
// from where it is MOUNTED, not from where its routes are declared — all 24 of
// them are mounted inside a group carrying CSRFMiddleware + VerifyAuth (and
// /admin additionally VerifyAdminAuthOnlyPostgres), which is what protects the
// ~550 routes declared on them. Nothing in the declaration of a route says
// whether it is protected.
//
// That makes the dangerous mistake invisible in a diff: mounting a new
// sub-router one line below the closing brace of the auth group exposes every
// route on it, and the mount line looks identical either way. Equally, adding
// `router.Get(...)` at the top level rather than on a sub-router silently
// publishes it. Neither shows up as a failing test, a compile error, or anything
// a reviewer scanning a large router file would reliably notice.
//
// So both properties are asserted against source: every mount sits inside an
// auth group, and the public route set matches the list below exactly. Adding a
// public endpoint therefore requires editing this list, which is the moment to
// justify it.
//
// EXACT match, not a subset, in both directions. A stale entry is worth failing
// on too: it means a route was renamed or removed and the list no longer
// describes the surface, which is how an allowlist quietly stops being a control.

// publicRoutes is every path intentionally reachable unauthenticated, with the
// reason it must be. Verified individually during the four-month audit.
var publicRoutes = map[string]string{
	// Sign-in and account recovery. Pre-auth by definition.
	"/auth/login": "credentials exchange",
	// The second half of a two-step sign-in, and unauthenticated by necessity: the caller has no
	// session yet, which is the entire reason this endpoint exists. It is not open, though — it
	// accepts nothing but a challenge token minted by /auth/login after a correct password, checked
	// for a purpose claim so a session token cannot be presented in its place, and it is rate limited
	// under its own key because it is the one place a six-digit secret can be guessed.
	"/auth/login/totp":                 "completes a 2FA challenge issued by /auth/login; no session exists yet",
	"/auth/passkey/begin":              "a passkey sign-in starts before anyone is signed in",
	"/auth/passkey/finish":             "a passkey sign-in is how someone becomes signed in",
	"/auth/signup":                     "invited-user registration",
	"/auth/ldap-login":                 "directory credentials exchange",
	"/auth/forgot-password":            "sends a reset mail; uniform response, no account oracle",
	"/auth/reset-password":             "consumes a mailed reset token",
	"/auth/validate-token":             "checks a mailed token before showing a form",
	"/auth/providers":                  "which sign-in methods to render on the login page",
	"/auth/admin-setup-required":       "first-boot check, before any account exists",
	"/auth/admin-setup":                "creates the first admin, before any account exists",
	"/demo-login":                      "demo deployments only",
	"/oauth_login/{oauth_provider}":    "starts an OAuth redirect",
	"/oauth_callback/{oauth_provider}": "receives the OAuth redirect",
	"/oauth_login/oidc":                "starts an OIDC redirect",
	"/oauth_callback/oidc":             "receives the OIDC redirect",
	"/saml/login":                      "starts a SAML redirect",
	"/saml/metadata":                   "SAML SP metadata, public by spec",
	"/saml/acs":                        "SAML assertion consumer; assertion is signature-verified",

	// OAuth callbacks for integrations: the provider redirects the browser here
	// with a state parameter, so they cannot require a session cookie.
	"/connector/oauth/callback":  "state-verified integration OAuth redirect",
	"/admin/apps/oauth/callback": "state-verified app-install OAuth redirect",

	// Inbound webhooks. Each authenticates the SENDER in its handler — a
	// signature or a token — because the caller is a third party with no session.
	"/integration/github/webhook":          "HMAC signature verified in the handler",
	"/slack/events":                        "Slack signing-secret HMAC verified in the handler; before connection it only echoes Slack's URL challenge",
	"/integration/google-calendar/webhook": "channel token verified in the handler",
	"/webhooks/resend":                     "Svix signature verified in the handler, refused without the secret; rate limited per address",
	"/webhook/incoming/{token}":            "the 32-byte path token IS the credential",

	// Guest share links. The token is the credential; every one is rate-limited
	// and returns a uniform not-available response so there is no oracle.
	"/public/form/{token}":                          "intake form: anyone with the link fills it in; rate-limited",
	"/public/book/{slug}":                           "booking page: outsiders see free slots and book; rate-limited",
	"/public/booking/{token}":                       "a guest's booking, by its cancel-link token",
	"/public/booking/{token}/cancel":                "a guest cancels through their cancel-link token",
	"/guest/channel/{token}":                        "share-link token",
	"/guest/channel/{token}/thread/{post_id}":       "share-link token",
	"/guest/project/{token}":                        "share-link token",
	"/guest/project/{token}/task/{task_id}":         "share-link token",
	"/guest/project/{token}/task/{task_id}/comment": "share-link token",
	"/guest/project/{token}/task/{task_id}/review":  "share-link token",
	"/guest/meet/{token}":                           "share-link token",
	"/guest/meet/{token}/join":                      "share-link token",
	"/guest/collab/{token}":                         "share-link token",
	"/guest/table/{token}":                          "share-link token",
	"/guest/doc-comments/{token}":                   "share-link token",
	"/guest/board-attachment/{token}/{obj_uuid}":    "share-link grant scopes the attachment",

	// Email-driven, must work from a mail client with no session.
	"/public/notifications/unsubscribe": "unsubscribe token; required by bulk-mail norms",
	"/public/notifications/resubscribe": "same token, reverses the above",

	// Public assets rendered in contexts with no session (emails, login page).
	// The browser posts CSP violation reports without credentials, and a
	// violation on the sign-in page happens before anybody is authenticated, so
	// this cannot require a session. It accepts nothing and returns nothing: the
	// body is capped, a batch is capped, every response is 204, and it is rate
	// limited under its own key. The worst an unauthenticated caller achieves is
	// a bounded number of WARN lines.
	"/public/csp-report": "browser-posted CSP violation reports; uncredentialed by spec",

	"/public/email/logo":          "branding in outbound mail",
	"/public/app-icon/{obj_uuid}": "app icon by opaque uuid",

	// Operational.
	"/health": "liveness probe",

	// Internal service-to-service. Not admin-gated because the caller is not a
	// user; authenticated by a shared runner token compared with
	// subtle.ConstantTimeCompare in business/AI/codePRLLMProxy.go.
	"/internal/code-run/llm": "shared runner token, constant-time compared",
	// The broker's question before each subscription. VerifyInternalServiceRequest:
	// the broker sends INTERNAL_SECRET with every ask.
	"/internal/mqtt/authorize": "internal secret, constant-time compared",
}

// mountsProtectedElsewhere are sub-routers deliberately mounted outside the auth
// group because their routes carry their own, stricter protection.
var mountsProtectedElsewhere = map[string]string{
	"/livekit":    "webhook is signature-verified; the rest use VerifyInternalServiceRequest",
	"/docColab":   "VerifyInternalServiceRequest on the collab service routes",
	"/boardColab": "VerifyInternalServiceRequest on the collab service routes",
}

var (
	rootRoute = regexp.MustCompile(`^\s*router\.(?:With\([^\n]*?\)\.)*(?:Get|Post|Put|Patch|Delete|Head|Options)\(\s*"([^"]*)"`)
	mountLine = regexp.MustCompile(`^(\s*)(?:r|router)\.Mount\(\s*"([^"]*)"\s*,\s*(\w+)`)
	groupOpen = regexp.MustCompile(`\.Group\(func\(`)
	// Auth applied to a group the mount sits inside (the session-cookie flow).
	useAuth = regexp.MustCompile(`r\.Use\((?:customMiddleware\.)?(VerifyAuth\w*|VerifyAdminAuth\w*)`)
	// Auth applied directly to the sub-router itself. /v1 does this: it is
	// authenticated by a scoped API token rather than a session cookie, so it is
	// legitimately mounted outside the cookie-auth group. Recognising the pattern
	// rather than allowlisting /v1 keeps the check live — deleting VerifyApiToken
	// from that router still fails this test.
	//
	// A CLOSED ALTERNATION, not `Verify\w+`. That is the point of it: a new
	// middleware whose name is not listed here fails the mount check until
	// somebody adds it, and adding it is the moment to confirm the thing being
	// named actually authenticates. A permissive pattern would accept
	// `VerifyBodyLimit` and protect nothing.
	//
	// VerifyScimToken is /scim/v2, authenticated by a workspace provisioning
	// credential. It could not reuse VerifyApiToken: that middleware refuses any
	// request whose token owner is not an active user, and deactivating users is
	// what SCIM is for, so a directory would lose access the moment it offboarded
	// whoever configured it.
	subRouterAuth = regexp.MustCompile(`^\s*(\w+)\.Use\((?:customMiddleware\.)?(VerifyAuth\w*|VerifyAdminAuth\w*|VerifyApiToken|VerifyScimToken|VerifyInternalServiceRequest)`)
	authTokens    = []string{"VerifyAuth", "VerifyAdminAuth", "VerifyApiToken", "VerifyScimToken"}
)

func routerSource(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("cannot read router.go: %v", err)
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) < 200 {
		t.Fatal("router.go looks truncated; this test would pass vacuously")
	}
	return lines
}

func TestPublicRouteSurfaceIsPinned(t *testing.T) {
	found := map[string]bool{}
	for _, line := range routerSource(t) {
		if m := rootRoute.FindStringSubmatch(line); m != nil {
			found[m[1]] = true
		}
	}
	if len(found) == 0 {
		t.Fatal("matched no top-level routes; the pattern no longer fits router.go")
	}

	var added, stale []string
	for p := range found {
		if _, ok := publicRoutes[p]; !ok {
			added = append(added, p)
		}
	}
	for p := range publicRoutes {
		if !found[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(added)
	sort.Strings(stale)

	for _, p := range added {
		t.Errorf("%s is registered on the ROOT router, so it is reachable with no "+
			"authentication. If that is intended, add it to publicRoutes with the "+
			"reason it must be public. If not, register it on the sub-router for its "+
			"area — those are mounted behind CSRFMiddleware + VerifyAuth.", p)
	}
	for _, p := range stale {
		t.Errorf("publicRoutes lists %s but no such root route exists. Remove the "+
			"entry — a list that no longer describes the surface has stopped being a "+
			"control.", p)
	}
}

func TestEverySubRouterMountIsBehindAuth(t *testing.T) {
	lines := routerSource(t)
	// Depth at which an auth-carrying group is open.
	type grp struct {
		depth int
		auth  bool
	}
	var stack []grp
	depth := 0
	mounts := 0

	// Sub-routers that apply an auth middleware to themselves, collected first
	// because the .Use line can appear far above the .Mount line.
	selfAuthed := map[string]bool{}
	for _, line := range lines {
		if m := subRouterAuth.FindStringSubmatch(line); m != nil {
			selfAuthed[m[1]] = true
		}
	}

	for i, line := range lines {
		if groupOpen.MatchString(line) {
			stack = append(stack, grp{depth: depth})
		}
		if useAuth.MatchString(line) && len(stack) > 0 {
			stack[len(stack)-1].auth = true
		}
		if m := mountLine.FindStringSubmatch(line); m != nil {
			path, routerVar := m[2], m[3]
			mounts++
			protected := selfAuthed[routerVar]
			for _, g := range stack {
				if g.auth {
					protected = true
				}
			}
			if !protected {
				if _, ok := mountsProtectedElsewhere[path]; !ok {
					t.Errorf("router.go:%d: %s is mounted outside any group carrying "+
						"VerifyAuth, so every route on that sub-router is reachable "+
						"unauthenticated. Move the mount inside the authenticated group, "+
						"or add it to mountsProtectedElsewhere with the protection it "+
						"relies on instead.", i+1, path)
				}
			}
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
		for len(stack) > 0 && depth <= stack[len(stack)-1].depth {
			stack = stack[:len(stack)-1]
		}
	}
	if mounts < 20 {
		t.Fatalf("only found %d mounts; expected ~27, so this test is no longer "+
			"inspecting the real router", mounts)
	}
}

// Guards the assumption the mount test rests on: that the auth middleware is
// still named what this file looks for. A rename would make the checks above
// pass while asserting nothing.
func TestAuthMiddlewareNamesStillExist(t *testing.T) {
	b, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("cannot read router.go: %v", err)
	}
	for _, name := range authTokens {
		if !strings.Contains(string(b), name) {
			t.Errorf("router.go no longer mentions %q. If the auth middleware was "+
				"renamed, update the patterns in this file — otherwise the mount and "+
				"public-surface checks silently stop protecting anything.", name)
		}
	}
}

// A route that changes something is never a GET. GETs skip the CSRF check and
// can't carry a body, and the app posts to these: /doc/deleteDoc was a GET,
// so every delete from the app answered 405.
func TestNoStateChangingGetRoutes(t *testing.T) {
	getChanges := regexp.MustCompile(`\.Get\("[^"]*",\s*\w+\.(Delete|Remove|Update|Create|Add|Set|Archive|Revoke|Leave|Join|Save|Mark|Send|Approve|Deny|Stop|Start)\w*\)`)
	// Handlers that must be GETs, and why.
	allowed := map[string]string{
		"connectorController.StartConnect": "begins an OAuth redirect: the browser navigates to it",
	}
	for i, line := range routerSource(t) {
		exempt := false
		for h := range allowed {
			exempt = exempt || strings.Contains(line, h+")")
		}
		if getChanges.MatchString(line) && !exempt {
			t.Errorf("router.go:%d registers a state-changing handler as GET: %s", i+1, strings.TrimSpace(line))
		}
	}
}

// Anyone can post to the Resend webhook. Its handler refuses what isn't
// signed, and the route is limited per address like the sign-in routes, so a
// stream of forged events costs little.
func TestResendWebhookIsRateLimited(t *testing.T) {
	for _, line := range routerSource(t) {
		if strings.Contains(line, `"/webhooks/resend"`) {
			if !strings.Contains(line, "RateLimit(") {
				t.Fatalf("/webhooks/resend has no rate limit: %s", strings.TrimSpace(line))
			}
			return
		}
	}
	t.Fatal("/webhooks/resend is not registered")
}
