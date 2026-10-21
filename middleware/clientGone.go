package middleware

import (
	"net/http"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ClientGone records a request whose client stopped waiting as 499, not as
// the 5xx its handler answered once its reads were called off.
//
// When a browser closes the task panel, or opens the next task before this
// one has loaded, net/http cancels the request's context; the Dgraph or
// Postgres read in flight fails with "context canceled", and the handler
// answers 503 to no one. On the demo every task-panel 5xx in a month was one
// of these, and each counted as a server failure in the traces, beside the
// failures that were real. It sits inside the tracing middleware: the span
// records 499 with http.client_gone, and its status is set to Ok, which the
// tracing middleware's own verdict can't overturn (its semconv calls 499 an
// invalid code, so an error). The log helper writes what those handlers log
// at DEBUG for the same reason (helpers.ClientWentAway). An answer the client
// is still waiting for is never changed.
func ClientGone(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		w = httpsnoop.Wrap(w, httpsnoop.Hooks{
			WriteHeader: func(write httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
				return func(code int) {
					if code >= http.StatusInternalServerError && helpers.ClientWentAway(ctx) {
						code = helpers.StatusClientClosedRequest
						span := trace.SpanFromContext(ctx)
						span.SetAttributes(attribute.Bool("http.client_gone", true))
						span.SetStatus(codes.Ok, "")
					}
					write(code)
				}
			},
		})
		next.ServeHTTP(w, r)
	})
}
