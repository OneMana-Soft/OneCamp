package router

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// A request is traced and logged by its route, never by what it was asked
// with. Its path carries ids and, on the guest and public routes, a link's
// token; its query can carry one (?token=); and the tracing records a Basic
// Authorization's user name as the span's enduser.id. otel_traces and
// otel_logs keep both for 30 days.
//
// Every route the server serves is asked with a marker in each path parameter,
// in ?token=, as a Basic user and password, and in a Referer, through the base
// middleware: answered, panicking, and, for a write, refused by the CSRF check
// for its origin and for its token. The marker must reach no span (its name,
// attributes, events or status) and no log line, and each refusal or panic is
// logged under its route.
//
// No route reads Basic, so the base middleware drops it (DropBasicAuth). If a
// route ever needs Basic, keep the header and drop enduser.id in a span
// processor instead.
func TestARequestIsTracedAndLoggedByItsRouteAlone(t *testing.T) {
	const marker = "qz7marker"
	const app = "https://app.acme.test"
	t.Setenv("CSRF_DISABLED", "")
	t.Setenv("CSRF_ALLOWED_ORIGINS", "")
	t.Setenv("FE_HOST_DOMAIN", "app.acme.test")
	t.Setenv("BACKEND_DOMAIN", "api.acme.test")

	recorder := tracetest.NewSpanRecorder()
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	var logged bytes.Buffer
	previousLogger := helpers.Logger
	helpers.Logger = slog.New(slog.NewJSONHandler(&logged, nil))
	t.Cleanup(func() { helpers.Logger = previousLogger })

	type asked struct{ method, pattern string }
	var served []asked
	if err := chi.Walk(routes(), func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served = append(served, asked{method, pattern})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(served) < 100 {
		t.Fatalf("the walk found %d routes: the guard would ask almost nothing", len(served))
	}

	passes := []struct {
		name   string
		csrf   bool
		panics bool
		header func(*http.Request)
		logs   string // what the pass's log line says, with the route
	}{
		{name: "answered"},
		{name: "panicking", panics: true, logs: "answered 500 after a panic"},
		{name: "refused for its origin", csrf: true, logs: "CSRF blocked: untrusted origin", header: func(r *http.Request) {
			r.Header.Set("Referer", "https://elsewhere.test/"+marker+"?token="+marker)
		}},
		{name: "refused for its token", csrf: true, logs: "CSRF blocked: cookie/header mismatch", header: func(r *http.Request) {
			r.Header.Set("Origin", app)
			r.Header.Set("Referer", app+"/guest/m/"+marker)
		}},
	}

	failures := map[string][]string{}
	fail := func(kind, where string) {
		failures[kind] = append(failures[kind], where)
	}
	for _, a := range served {
		for _, p := range passes {
			if p.csrf && (a.method == http.MethodGet || a.method == http.MethodHead || a.method == http.MethodOptions) {
				continue // the CSRF check reads writes only
			}
			where := a.method + " " + a.pattern + " (" + p.name + ")"
			reached := false
			var endpoint http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				if p.panics {
					panic("the handler failed")
				}
				w.WriteHeader(http.StatusNoContent)
			})
			if p.csrf {
				endpoint = customMiddleware.CSRFMiddleware(endpoint)
			}
			r := chi.NewRouter()
			useBaseMiddleware(r, []string{app})
			r.Method(a.method, a.pattern, endpoint)

			req := httptest.NewRequest(a.method, pathWith(a.pattern, marker, marker)+"?token="+marker, nil)
			req.SetBasicAuth(marker, marker)
			if p.header != nil {
				p.header(req)
			}
			spansBefore, logBefore := len(recorder.Ended()), logged.Len()
			r.ServeHTTP(httptest.NewRecorder(), req)

			if reached == p.csrf {
				fail("not asked as the pass means (the route wasn't reached, or the CSRF check let it through)", where)
				continue
			}
			for _, s := range recorder.Ended()[spansBefore:] {
				if strings.Contains(s.Name(), marker) {
					fail("span name", where)
				}
				for _, kv := range s.Attributes() {
					if strings.Contains(kv.Value.Emit(), marker) {
						fail("span attribute "+string(kv.Key), where)
					}
				}
				for _, e := range s.Events() {
					text := e.Name
					for _, kv := range e.Attributes {
						text += " " + kv.Value.Emit()
					}
					if strings.Contains(text, marker) {
						fail("span event "+e.Name, where)
					}
				}
				if strings.Contains(s.Status().Description, marker) {
					fail("span status", where)
				}
			}
			lines := logged.Bytes()[logBefore:]
			if bytes.Contains(lines, []byte(marker)) {
				fail("log line", where)
			}
			if p.logs != "" && !loggedUnder(lines, p.logs, a.method+" "+routeName(a.pattern)) {
				fail("no log line naming the route", where)
			}
		}
	}
	for kind, where := range failures {
		if len(where) > 3 {
			where = append(where[:3], "...")
		}
		t.Errorf("%s: %d asks, e.g.\n  %s", kind, len(failures[kind]), strings.Join(where, "\n  "))
	}
}

// routeName is the pattern as chi names a matched route (RoutePattern): with
// no trailing slash, but for the root.
func routeName(pattern string) string {
	if pattern == "/" {
		return pattern
	}
	return strings.TrimSuffix(pattern, "/")
}

// loggedUnder: one of lines (JSON, one a line) says what and names route.
func loggedUnder(lines []byte, what, route string) bool {
	for _, raw := range bytes.Split(lines, []byte("\n")) {
		var line struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal(raw, &line) == nil && strings.Contains(line.Msg, what) && strings.Contains(line.Msg, route) {
			return true
		}
	}
	return false
}
