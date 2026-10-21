package middleware

import (
	"errors"
	"net/http"
	"runtime/debug"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// panicMessage is what the person reads when a handler panicked. The app shows
// a 5xx's msg in its toast; without one it can only say "The server hit a
// problem", which is all chi's Recoverer ever gave it (an empty 500).
const panicMessage = "Something broke on the server while doing that. It has been logged; try again in a moment."

// RecoverPanic turns a handler's panic into a 500 the app can read, and a log
// line that says where it happened.
//
// It sits INSIDE the tracing middleware, unlike chi's Recoverer. A panic that
// unwound through otelchi ended the request's span with no route and no status,
// and chi's Recoverer printed the stack to stderr, which `docker logs` loses on
// every rebuild: the failure left nothing in otel_traces or otel_logs to find.
// Here one ERROR line carries the route, the status, the panic and its stack,
// with the request's trace id (LogErrorWithContext), and the span records the
// 500 under its route.
//
// http.ErrAbortHandler is re-panicked: net/http uses it to abort a response on
// purpose, and recovering it would turn that into a 500. A WebSocket upgrade
// gets no body, since the connection is no longer HTTP's to write to.
func RecoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			helpers.LogErrorWithContext(r.Context(), "middleware/RecoverPanic %s %s answered %d after a panic: %v\n%s",
				r.Method, routeOf(r), http.StatusInternalServerError, rec, debug.Stack())
			if r.Header.Get("Connection") == "Upgrade" {
				return
			}
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": panicMessage})
		}()
		next.ServeHTTP(w, r)
	})
}

// routeOf is the route a request matched ("/task/info/{task_uuid}"), which
// groups in the logs where the path, with its ids, doesn't; the path when it
// matched none.
func routeOf(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if pattern := rctx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return r.URL.Path
}
