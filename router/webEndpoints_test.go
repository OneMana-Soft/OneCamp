package router

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Every endpoint the web app names or calls is served here (REGRESSION-AUDIT,
// 24 Oct 2026). A button whose request no route serves looks fine and does
// nothing: its request answers 404 or 405, which the app shows, at best, as a
// toast nobody can act on. Each side's own tests pass meanwhile, since each
// pins only its own half.
//
// The web names its endpoints in enums, one per method kind (services/
// endPoints.ts, and services/endpoints/<area>.ts since the split of 18 Oct):
// an enum named …Get is read with GET, any other is written (POST, PUT, PATCH
// or DELETE). Its services also name paths in objects and constants of their
// own, and some calls write a path out where they are made (axiosInstance's
// methods, useFetch, a write's apiEndpoint). Each must lead to a route of its
// kind: its path, or the start of one, since the app appends an id, or an id
// and an action.
func TestEveryEndpointTheWebAppNamesIsServed(t *testing.T) {
	fe := os.Getenv("ONECAMP_FE_DIR")
	if fe == "" {
		t.Skip("ONECAMP_FE_DIR names the web app to check against")
	}
	served := servedRoutes(t)
	named := webEndpoints(t, fe)
	if n := len(named); n < 400 {
		t.Fatalf("read %d endpoints from the web app, which names more: the reading below is wrong", n)
	}
	var missed []string
	for _, e := range named {
		if !servedBy(served, e) {
			missed = append(missed, e.String())
		}
	}
	sort.Strings(missed)
	for _, m := range missed {
		t.Error(m)
	}
}

// The reading itself: an enum's kind, an object's and a call's paths, a
// template's static start, and a path that leads to a route or doesn't.
func TestTheWebAppsEndpointsAreReadAsWritten(t *testing.T) {
	fe := t.TempDir()
	for file, src := range map[string]string{
		"services/endPoints.ts":          "export enum GetEndpointUrl {\n    Me = \"/user/me\",\n}\n\nexport enum PostEndpointUrl {\n    Save = '/user/save', // a comment\n}\n",
		"services/endpoints/calendar.ts": "export enum CalendarGet {\n    Pages = \"/event/bookingPages\",\n}\n",
		"services/docGraphService.ts":    "export const DocGraphEndpoint = {\n  Purge: \"/doc/purge\",\n} as const\nexport const THREADS_PATH = '/threads'\n",
		"hooks/useThing.ts":              "const a = await axiosInstance.post(`/thing/${id}/archive`, {})\nconst b = useFetch<X>('/thing/list?page=1')\nmakeRequest({ apiEndpoint: \"/thing/read\", method: \"GET\" })\n",
		"components/thing.test.tsx":      "axiosInstance.get('/never/read')\n",
		"components/thing/ThingCard.tsx": "useFetch(`${GetEndpointUrl.Me}/x`)\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(fe, file)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fe, file), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, e := range webEndpoints(t, fe) {
		got = append(got, e.method+" "+e.path)
	}
	sort.Strings(got)
	want := []string{"GET /event/bookingPages", "GET /thing/list", "GET /thing/read", "GET /user/me", "WRITE /thing/", "WRITE /user/save", "any /doc/purge", "any /threads"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("read %v\nwant %v", got, want)
	}

	routes := []route{
		{http.MethodGet, segmentsOf("/user/me")},
		{http.MethodPost, segmentsOf("/thing/{id}/archive")},
		{http.MethodDelete, segmentsOf("/doc/purge")},
		{http.MethodGet, segmentsOf("/files/*")},
	}
	for _, c := range []struct {
		method, path string
		served       bool
	}{
		{http.MethodGet, "/user/me", true},
		{"WRITE", "/user/me", false},
		{"WRITE", "/thing", true},
		{"WRITE", "/thing/t1/archive", true},
		{"WRITE", "/thing/t1/delete", false},
		{"any", "/doc/purge", true},
		{http.MethodGet, "/files/a/b", true},
		{http.MethodGet, "/user/you", false},
	} {
		if got := servedBy(routes, webEndpoint{method: c.method, path: c.path}); got != c.served {
			t.Errorf("%s %s served %v, want %v", c.method, c.path, got, c.served)
		}
	}
}

// route is one route of this server: its method and its path's segments.
type route struct {
	method   string
	segments []string
}

// servedRoutes is every route the server serves.
func servedRoutes(t *testing.T) []route {
	t.Helper()
	var out []route
	if err := chi.Walk(routes(), func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, route{method: method, segments: segmentsOf(pattern)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// webEndpoint is one endpoint the web app names or calls: where, its path, and
// how it is asked: GET, WRITE (any other method) or any.
type webEndpoint struct {
	at, path, method string
}

func (e webEndpoint) String() string {
	return e.at + ": " + e.method + " " + e.path + ": no route serves it"
}

var (
	webEnum      = regexp.MustCompile(`(?s)export enum (\w+) \{(.*?)\n\}`)
	webEnumValue = regexp.MustCompile(`(?m)^\s*(\w+)\s*=\s*["'](/[^"']*)["']`)
	webObject    = regexp.MustCompile(`(?s)export const \w+ = \{(.*?)\n\}`)
	webObjectKey = regexp.MustCompile(`(?m)^\s*\w+:\s*["'](/[^"']*)["']`)
	webConstant  = regexp.MustCompile(`(?m)^export const [A-Z][A-Z0-9_]* = ["'](/[^"']*)["']`)
	// A path written at a call: axiosInstance's methods (their method), useFetch
	// and its kin (GET), a write's apiEndpoint (GET when its options say so).
	webCall      = regexp.MustCompile("(?:axiosInstance\\.(get|post|put|patch|delete)(?:<[^>()]*>)?\\(|\\b(?:useFetch|useFetchOnlyOnce|useSWR)(?:<[^>()]*>)?\\(|(apiEndpoint):)\\s*[`'\"](/[^`'\"]*)")
	webGetOption = regexp.MustCompile(`^[^}]*method:\s*["']GET["']`)
)

// webEndpoints reads every endpoint the web app names in its services and
// every path its code writes out at a call.
func webEndpoints(t *testing.T, fe string) []webEndpoint {
	t.Helper()
	var out []webEndpoint
	for _, dir := range []string{"services", "hooks", "lib", "components", "app"} {
		root := filepath.Join(fe, dir)
		if _, err := os.Stat(root); err != nil {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			name := d.Name()
			if !(strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".tsx")) || strings.Contains(name, ".test.") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(fe, path)
			out = append(out, endpointsIn(rel, string(src), dir == "services")...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// endpointsIn is the endpoints one file names: in a service, its enums,
// objects and constants; anywhere, the paths written at its calls.
func endpointsIn(file, src string, service bool) []webEndpoint {
	var out []webEndpoint
	at := func(i int) string { return file + ":" + strconv.Itoa(strings.Count(src[:i], "\n")+1) }
	if service {
		for _, enum := range webEnum.FindAllStringSubmatchIndex(src, -1) {
			method := "WRITE"
			if strings.HasSuffix(src[enum[2]:enum[3]], "Get") || src[enum[2]:enum[3]] == "GetEndpointUrl" {
				method = http.MethodGet
			}
			body := src[enum[4]:enum[5]]
			for _, v := range webEnumValue.FindAllStringSubmatchIndex(body, -1) {
				out = append(out, webEndpoint{at: at(enum[4] + v[0]), path: body[v[4]:v[5]], method: method})
			}
		}
		for _, obj := range webObject.FindAllStringSubmatchIndex(src, -1) {
			body := src[obj[2]:obj[3]]
			for _, v := range webObjectKey.FindAllStringSubmatchIndex(body, -1) {
				out = append(out, webEndpoint{at: at(obj[2] + v[0]), path: body[v[2]:v[3]], method: "any"})
			}
		}
		for _, c := range webConstant.FindAllStringSubmatchIndex(src, -1) {
			out = append(out, webEndpoint{at: at(c[0]), path: src[c[2]:c[3]], method: "any"})
		}
	}
	for _, c := range webCall.FindAllStringSubmatchIndex(src, -1) {
		path := src[c[6]:c[7]]
		// A template's static start, before its first ${…}; a query goes.
		if i := strings.Index(path, "${"); i >= 0 {
			path = path[:i]
		}
		method := "WRITE"
		switch {
		case c[2] >= 0 && src[c[2]:c[3]] == "get", c[2] < 0 && c[4] < 0:
			method = http.MethodGet
		case c[4] >= 0 && webGetOption.MatchString(src[c[1]:min(len(src), c[1]+300)]):
			method = http.MethodGet
		}
		out = append(out, webEndpoint{at: at(c[0]), path: strings.SplitN(path, "?", 2)[0], method: method})
	}
	return out
}

// segmentsOf is a path's segments, without its query or a trailing slash.
func segmentsOf(path string) []string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

// servedBy is whether a route of the endpoint's kind has its path, or starts
// with it: each segment the same, or a parameter ({id}) where the route has
// one, or anything under a mount's wildcard.
func servedBy(routes []route, e webEndpoint) bool {
	want := segmentsOf(e.path)
	for _, r := range routes {
		switch e.method {
		case http.MethodGet:
			if r.method != http.MethodGet {
				continue
			}
		case "WRITE":
			if r.method == http.MethodGet {
				continue
			}
		}
		if leadsTo(want, r.segments) {
			return true
		}
	}
	return false
}

func leadsTo(want, route []string) bool {
	for i, w := range want {
		if i >= len(route) {
			return false
		}
		s := route[i]
		switch {
		case s == "*":
			return true
		case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
			continue
		case s != w:
			return false
		}
	}
	return true
}
