package router

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// Every route the server answers says who may call it, in
// testdata/route-access.tsv: one line per method and pattern, sorted by
// pattern then method, its columns tab-separated:
//
//	METHOD PATTERN GATE TOKEN DEMO CHECK REFUSAL NOTE
//
// GATE is what the route's middleware lets through, worked out from the
// chain the router really applies, so no line can claim a gate it lacks:
//
//	public    nothing: the handler, a link's token or a signature decides
//	session   a signed-in member (VerifyAuth)
//	admin     a signed-in workspace admin (VerifyAdminAuthOnlyPostgres)
//	token     an API token (VerifyApiToken: /v1)
//	scim      a directory's SCIM token
//	internal  the server's own services (VerifyInternalServiceRequest)
//	refresh   a refresh token (a session renewed)
//
// TOKEN is what an API token may do there: none (no token passes the gate),
// as-member (it acts as its member, never more) or refused. Only token lines
// say anything but none.
//
// DEMO is the shared demo's visitor, also from the chain: member (they act as
// the member they are), refused (an admin's gate, or notForDemoVisitor),
// public, or n/a (a gate the visitor has no credential for).
//
// CHECK is what is checked past the gate, from a closed vocabulary:
//
//	public             open by design (health, signing in); public lines only
//	link:<kind>        a link's own token decides; public lines only
//	signed:<sender>    the sender's signature decides (a webhook); public lines only
//	gate               the gate is the whole check (admin, scim, internal, refresh)
//	member             any member may, by design
//	self               the caller's own records, and nobody else's
//	admin              the handler refuses anyone but a workspace admin
//	capability:<name>  RequireCapability(<name>) in the chain (asked of the chain)
//	object:<kind>      the handler checks the caller's access to that object
//	TODO(<area>)       not checked yet: their count only goes down
//
// REFUSAL names the test where a lower role is refused or sees nothing, or
// says "gate" where the gate's own refusal is the proof (the sweep below for
// a caller with nothing, the middleware's tests for a lower role); "-" only
// on a TODO line or a public one. NOTE says why, a public line's especially.
//
// A new route fails here until it has its line. ROUTE_ACCESS_WRITE=1 writes
// the file again from the router: lines kept as they are, gone routes taken
// out, new ones added as TODO, their gate and demo worked out. A new TODO
// still fails the ceiling: give it its check and refusal.
//
// This replaced router/publicSurface_test.go's publicRoutes map (25 Oct
// 2026): the public lines, with their reasons, are the public surface, and
// they are read off the chain, so a route on a sub-router mounted without
// auth is public here too, which the root-route regex never saw.

const routeAccessPath = "testdata/route-access.tsv"

// todoCeiling is how many lines still say TODO. It only goes down.
const todoCeiling = 518

type accessLine struct {
	Method, Pattern, Gate, Token, Demo, Check, Refusal, Note string
}

func (l accessLine) key() string { return l.Pattern + " " + l.Method }

func (l accessLine) String() string {
	return strings.Join([]string{l.Method, l.Pattern, l.Gate, l.Token, l.Demo, l.Check, l.Refusal, l.Note}, "\t")
}

// walkedRoute is a route as the router applies it.
type walkedRoute struct {
	method, pattern string
	gate, demo      string
	capabilities    map[string]bool
}

// gateMiddleware names each gate's middleware, the stronger gate first: an
// admin's routes pass VerifyAuth too. VerifyRefreshTokenForLogout isn't one:
// signing out lets every caller through, to clear their cookies. VerifyDocColabAuth is the session the
// collaboration service presents for a member (their own access token,
// checked as VerifyAuth checks the cookie); behind the internal secret too,
// a route is the service's alone.
var gateMiddleware = []struct{ name, gate string }{
	{"middleware.VerifyAdminAuthOnlyPostgres", "admin"},
	{"middleware.VerifyAuth", "session"},
	{"middleware.VerifyAuthOnlyPostgres", "session"},
	{"middleware.VerifyApiToken", "token"},
	{"middleware.VerifyScimToken", "scim"},
	{"middleware.VerifyInternalServiceRequest", "internal"},
	{"middleware.VerifyDocColabAuth", "session"},
	{"middleware.VerifyRefreshToken", "refresh"},
}

func funcName(f any) string {
	fn := runtime.FuncForPC(reflect.ValueOf(f).Pointer())
	if fn == nil {
		return ""
	}
	return strings.TrimPrefix(fn.Name(), "github.com/akashc777/OneCamp/")
}

// capabilityOf asks a RequireCapability middleware which capability it
// requires: a bot is never granted one, so it refuses, and its refusal names it.
func capabilityOf(mw func(http.Handler) http.Handler) string {
	var bot userModels.UserInfo
	bot.UserPostgresInfo.IsBot = true
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, bot))
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req)
	var body struct {
		Capability string `json:"capability"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Capability
}

func walkRoutes(t *testing.T) []walkedRoute {
	t.Helper()
	var out []walkedRoute
	err := chi.Walk(routerMux(), func(method, pattern string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		r := walkedRoute{method: method, pattern: pattern, gate: "public", capabilities: map[string]bool{}}
		names := map[string]bool{}
		demoRefused := false
		for _, mw := range mws {
			n := funcName(mw)
			names[n] = true
			if strings.Contains(n, "notForDemoVisitor") {
				demoRefused = true
			}
			if strings.Contains(n, "RequireCapability") {
				r.capabilities[capabilityOf(mw)] = true
			}
		}
		for _, g := range gateMiddleware {
			if names[g.name] {
				r.gate = g.gate
				break
			}
		}
		switch {
		case r.gate == "admin" || demoRefused:
			r.demo = "refused"
		case r.gate == "session":
			r.demo = "member"
		case r.gate == "public":
			r.demo = "public"
		default:
			r.demo = "n/a"
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the router: %v", err)
	}
	if len(out) < 500 {
		t.Fatalf("the router has %d routes; this test would pass vacuously", len(out))
	}
	return out
}

func readAccessLines(t *testing.T) []accessLine {
	t.Helper()
	f, err := os.Open(routeAccessPath)
	if err != nil {
		t.Fatalf("reading %s: %v", routeAccessPath, err)
	}
	defer f.Close()
	var lines []accessLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		text := sc.Text()
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		cols := strings.Split(text, "\t")
		if len(cols) == 7 {
			cols = append(cols, "")
		}
		if len(cols) != 8 {
			t.Fatalf("%s:%d has %d columns, want METHOD PATTERN GATE TOKEN DEMO CHECK REFUSAL NOTE", routeAccessPath, n, len(cols))
		}
		lines = append(lines, accessLine{cols[0], cols[1], cols[2], cols[3], cols[4], cols[5], cols[6], cols[7]})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// testNames is every Go test function in the repository, so a REFUSAL names one that exists.
func testNames(t *testing.T) map[string]bool {
	t.Helper()
	def := regexp.MustCompile(`(?m)^func (Test\w+)\(t \*testing\.T\)`)
	names := map[string]bool{}
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "other-services" || strings.HasPrefix(d.Name(), ".")) && path != ".." {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range def.FindAllSubmatch(b, -1) {
			names[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("listing the repository's tests: %v", err)
	}
	return names
}

var (
	checkWord   = regexp.MustCompile(`^(public|gate|member|self|admin|(link|signed|capability|object):[a-z0-9_.-]+|TODO\([a-z0-9-]+\))$`)
	objectKinds = map[string]bool{
		"agent": true, "app": true, "approval": true, "attachment": true, "board": true, "booking": true, "calendar": true,
		"call": true, "channel": true, "chat": true, "comment": true, "connector": true, "conversation": true,
		"cycle": true, "data-source": true, "doc": true, "event": true, "file": true, "form": true, "goal": true,
		"group-chat": true, "guest-link": true, "post": true, "project": true, "proposal": true, "report": true, "skill": true,
		"table": true, "task": true, "team": true, "template": true, "thread": true, "token": true, "webhook": true,
		"workflow": true,
	}
)

func TestEveryRouteDeclaresItsAccess(t *testing.T) {
	routes := walkRoutes(t)
	if os.Getenv("ROUTE_ACCESS_WRITE") == "1" {
		writeAccessLines(t, routes)
	}
	lines := readAccessLines(t)
	if !sort.SliceIsSorted(lines, func(i, j int) bool { return lines[i].key() < lines[j].key() }) {
		t.Errorf("%s isn't sorted by pattern, then method", routeAccessPath)
	}
	declared := map[string]accessLine{}
	for _, l := range lines {
		if _, twice := declared[l.key()]; twice {
			t.Errorf("%s %s has two lines", l.Method, l.Pattern)
		}
		declared[l.key()] = l
	}
	tests := testNames(t)
	todos := 0
	routed := map[string]bool{}
	for _, r := range routes {
		k := r.pattern + " " + r.method
		routed[k] = true
		l, ok := declared[k]
		if !ok {
			t.Errorf("%s %s has no line in %s: say who may call it (ROUTE_ACCESS_WRITE=1 adds it as a TODO to fill in)", r.method, r.pattern, routeAccessPath)
			continue
		}
		where := r.method + " " + r.pattern
		if l.Gate != r.gate {
			t.Errorf("%s: GATE says %s, but its middleware makes it %s", where, l.Gate, r.gate)
		}
		if l.Demo != r.demo {
			t.Errorf("%s: DEMO says %s, but its middleware makes it %s", where, l.Demo, r.demo)
		}
		switch {
		case r.gate == "token" && l.Token != "as-member" && l.Token != "refused":
			t.Errorf("%s: an API token passes its gate, so TOKEN is as-member or refused, not %q", where, l.Token)
		case r.gate != "token" && l.Token != "none":
			t.Errorf("%s: no API token passes its gate, so TOKEN is none, not %q", where, l.Token)
		}
		if !checkWord.MatchString(l.Check) {
			t.Errorf("%s: CHECK %q isn't in the vocabulary (see the top of this file)", where, l.Check)
		}
		kind, arg, _ := strings.Cut(l.Check, ":")
		switch {
		case strings.HasPrefix(l.Check, "TODO("):
			todos++
		case l.Check == "public" || kind == "link" || kind == "signed":
			if r.gate != "public" {
				t.Errorf("%s: CHECK %s is for a public line, and this one's gate is %s", where, l.Check, r.gate)
			}
		case l.Check == "gate":
			if r.gate == "public" || r.gate == "session" || r.gate == "token" {
				t.Errorf("%s: a %s gate lets in more than one kind of caller, so it can't be the whole check", where, r.gate)
			}
		case kind == "capability":
			if !r.capabilities[arg] {
				t.Errorf("%s: CHECK says capability %s, but its chain requires %v", where, arg, keys(r.capabilities))
			}
		case kind == "object":
			if !objectKinds[arg] {
				t.Errorf("%s: %s isn't an object kind this file knows; add it to objectKinds", where, arg)
			}
		}
		if r.gate == "public" && !(l.Check == "public" || kind == "link" || kind == "signed" || strings.HasPrefix(l.Check, "TODO(")) {
			t.Errorf("%s: nothing gates it, so its CHECK is public, link:<kind> or signed:<sender>, not %s", where, l.Check)
		}
		switch l.Refusal {
		case "-":
			if r.gate != "public" && !strings.HasPrefix(l.Check, "TODO(") {
				t.Errorf("%s: REFUSAL - is only for a TODO or public line: name the test where a lower role is refused", where)
			}
		case "gate":
			if r.gate == "public" {
				t.Errorf("%s: nothing gates it, so its REFUSAL can't be the gate", where)
			}
		default:
			if !tests[l.Refusal] {
				t.Errorf("%s: REFUSAL names %s, and no test has that name", where, l.Refusal)
			}
		}
		if r.gate == "public" && l.Note == "" {
			t.Errorf("%s: anyone can call it, so its NOTE says why", where)
		}
	}
	for _, l := range lines {
		if !routed[l.key()] {
			t.Errorf("%s lists %s %s, and the router has no such route: take the line out", routeAccessPath, l.Method, l.Pattern)
		}
	}
	switch {
	case todos > todoCeiling:
		t.Errorf("%d lines say TODO, more than the %d left: a new route says who may call it, with the test that refuses a lower role", todos, todoCeiling)
	case todos < todoCeiling:
		t.Errorf("%d lines say TODO now, fewer than %d: lower todoCeiling to %d", todos, todoCeiling, todos)
	}
}

// Nothing behind a gate answers a caller with no credentials but a refusal:
// a 401 or a 403, never a page (2xx), a redirect, or a fault (5xx).
func TestNoRouteAnswersAnonymousCallers(t *testing.T) {
	t.Setenv("JWT_SECRET", "route-access-sweep")
	t.Setenv("INTERNAL_SECRET", "route-access-sweep-internal")
	quietLogs()
	mux := routerMux()
	asked := 0
	for _, r := range walkRoutes(t) {
		if r.gate == "public" {
			continue
		}
		asked++
		req := httptest.NewRequest(r.method, examplePath(r.pattern), strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("%s %s answered a caller with no credentials %d; its %s gate should refuse with 401 or 403", r.method, r.pattern, rec.Code, r.gate)
		}
	}
	if asked < 500 {
		t.Fatalf("asked %d routes; this test would pass vacuously", asked)
	}
}

var routeParam = regexp.MustCompile(`\{([^}:]+)(?::([^}]+))?\}`)

// examplePath fills a pattern's parameters with values each one accepts.
func examplePath(pattern string) string {
	return pathWith(pattern, "00000000-0000-4000-8000-000000000000", "x")
}

// pathWith fills pattern's plain parameters with value and its * with rest. A
// {name:regexp} parameter takes the first of value, the regexp's first
// alternative ("cover" of cover|logo) and a few stock values that its regexp
// accepts.
func pathWith(pattern, value, rest string) string {
	p := routeParam.ReplaceAllStringFunc(pattern, func(param string) string {
		m := routeParam.FindStringSubmatch(param)
		if m[2] == "" {
			return value
		}
		re := regexp.MustCompile("^(?:" + m[2] + ")$")
		first := strings.SplitN(m[2], "|", 2)[0]
		for _, v := range []string{value, first, "00000000-0000-4000-8000-000000000000", "1", "x", "a.png"} {
			if re.MatchString(v) {
				return v
			}
		}
		return "x"
	})
	return strings.ReplaceAll(p, "/*", "/"+rest)
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeAccessLines writes the file again from the router: each line kept as
// it is, routes gone taken out, new ones added with their gate and demo and
// a check to fill in.
func writeAccessLines(t *testing.T, routes []walkedRoute) {
	t.Helper()
	kept := map[string]accessLine{}
	if _, err := os.Stat(routeAccessPath); err == nil {
		for _, l := range readAccessLines(t) {
			kept[l.key()] = l
		}
	}
	var lines []accessLine
	for _, r := range routes {
		l, ok := kept[r.pattern+" "+r.method]
		if !ok {
			l = newAccessLine(r)
		}
		l.Gate, l.Demo = r.gate, r.demo
		lines = append(lines, l)
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].key() < lines[j].key() })
	var b strings.Builder
	b.WriteString("# Who may call each route: see router/routeAccess_test.go for the columns and their words.\n")
	b.WriteString("# METHOD\tPATTERN\tGATE\tTOKEN\tDEMO\tCHECK\tREFUSAL\tNOTE\n")
	for _, l := range lines {
		fmt.Fprintln(&b, l.String())
	}
	if err := os.WriteFile(routeAccessPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newAccessLine is a new route's first line: its check a TODO unless the
// chain says it (an admin's or a service's gate, one capability).
func newAccessLine(r walkedRoute) accessLine {
	l := accessLine{Method: r.method, Pattern: r.pattern, Token: "none", Check: "TODO(" + areaOf(r.pattern) + ")", Refusal: "-"}
	switch {
	case r.gate == "token":
		l.Token = "as-member"
	case r.gate == "admin" || r.gate == "scim" || r.gate == "internal" || r.gate == "refresh":
		l.Check, l.Refusal = "gate", "gate"
	case len(r.capabilities) == 1:
		l.Check, l.Refusal = "capability:"+keys(r.capabilities)[0], "gate"
	}
	return l
}

func areaOf(pattern string) string {
	seg, _, _ := strings.Cut(strings.TrimPrefix(pattern, "/"), "/")
	seg = strings.ToLower(regexp.MustCompile(`[^a-zA-Z0-9-]+`).ReplaceAllString(seg, "-"))
	if seg == "" {
		return "root"
	}
	return strings.Trim(seg, "-")
}
