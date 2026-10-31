package router

import (
	"os"
	"path/filepath"
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
// So both properties are asserted: every mount sits inside an auth group
// (against source, below), and the public route set is pinned in
// testdata/route-access.tsv, whose public lines say why each must be open
// (routeAccess_test.go reads every route's gate off the chain the router really
// applies, so a route on a sub-router mounted without auth is public there too).
// Adding a public endpoint therefore requires its line, which is the moment to
// justify it. The match is exact in both directions: a stale line fails too,
// since a list that no longer describes the surface has stopped being a control.

// mountsProtectedElsewhere are sub-routers deliberately mounted outside the auth
// group because each of their routes carries its own check. The ones public by
// their chain refuse every caller their token doesn't name
// (router/liveSessionAuth_test.go, tests/integration/live_session_tokens_test.go).
var mountsProtectedElsewhere = map[string]string{
	"/livekit":    "the webhook takes only events signed with the server's LiveKit secret; transcript ingestion, the agent's heartbeat and the transcription config take the internal secret; a member's own transcript, capability and transcription switch take a session",
	"/docColab":   "authorize takes the member's own access token (VerifyDocColabAuth), then the doc's rule; guestAuthorize takes a guest's collab token, checked against its grant; the rest take the internal secret",
	"/boardColab": "authorize takes the member's own access token (VerifyDocColabAuth), then the board's rule; guestAuthorize takes a guest's collab token, checked against its grant; the rest take the internal secret",
}

var (
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

// routeFile is one file of this package that declares routes.
type routeFile struct {
	name  string
	lines []string
}

// routeFiles are the files that declare routes: router.go first, then every
// other non-test file in this package. The docs programme declares its routes
// beside router.go (docsProgramme.go, and a docs<Area>.go per package) rather
// than in it, so the checks here read them all: a route declared through the
// programme meets the same controls as one in router.go.
func routeFiles(t *testing.T) []routeFile {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("cannot list the router package: %v", err)
	}
	sort.Strings(names)
	files := []routeFile{{name: "router.go"}}
	for _, n := range names {
		if n != "router.go" && !strings.HasSuffix(n, "_test.go") {
			files = append(files, routeFile{name: n})
		}
	}
	for i := range files {
		b, err := os.ReadFile(files[i].name)
		if err != nil {
			t.Fatalf("cannot read %s: %v", files[i].name, err)
		}
		files[i].lines = strings.Split(string(b), "\n")
	}
	if len(files[0].lines) < 200 {
		t.Fatal("router.go looks truncated; this test would pass vacuously")
	}
	return files
}

// routerSource is every line of every route file, router.go's first.
func routerSource(t *testing.T) []string {
	t.Helper()
	var lines []string
	for _, f := range routeFiles(t) {
		lines = append(lines, f.lines...)
	}
	return lines
}

func TestEverySubRouterMountIsBehindAuth(t *testing.T) {
	files := routeFiles(t)
	// Depth at which an auth-carrying group is open.
	type grp struct {
		depth int
		auth  bool
	}
	mounts := 0

	// Sub-routers that apply an auth middleware to themselves, collected first
	// because the .Use line can appear far above the .Mount line.
	selfAuthed := map[string]bool{}
	for _, line := range routerSource(t) {
		if m := subRouterAuth.FindStringSubmatch(line); m != nil {
			selfAuthed[m[1]] = true
		}
	}

	// Each file on its own: a group never spans two files.
	for _, f := range files {
		var stack []grp
		depth := 0
		for i, line := range f.lines {
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
						t.Errorf("%s:%d: %s is mounted outside any group carrying "+
							"VerifyAuth, so every route on that sub-router is reachable "+
							"unauthenticated. Move the mount inside the authenticated group, "+
							"or add it to mountsProtectedElsewhere with the protection it "+
							"relies on instead.", f.name, i+1, path)
					}
				}
			}
			depth += strings.Count(line, "{") - strings.Count(line, "}")
			for len(stack) > 0 && depth <= stack[len(stack)-1].depth {
				stack = stack[:len(stack)-1]
			}
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
	for _, f := range routeFiles(t) {
		for i, line := range f.lines {
			exempt := false
			for h := range allowed {
				exempt = exempt || strings.Contains(line, h+")")
			}
			if getChanges.MatchString(line) && !exempt {
				t.Errorf("%s:%d registers a state-changing handler as GET: %s", f.name, i+1, strings.TrimSpace(line))
			}
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
