package middleware

import "net/http"

// DropBasicAuth removes an Authorization header in the Basic scheme before the
// request is traced. No route reads one: API tokens, MCP clients, call guests
// and the collab server all send Bearer. The tracing, though, records a Basic
// user name as the span's enduser.id, so a client that sent a token as one
// would have it kept with the trace. If a route ever needs Basic, keep the
// header and drop enduser.id in a span processor instead.
func DropBasicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); ok {
			r.Header.Del("Authorization")
		}
		next.ServeHTTP(w, r)
	})
}
