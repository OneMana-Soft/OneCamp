package middleware

import (
	"net/http"

	"github.com/akashc777/OneCamp/helpers"
)

// BodyLimit caps the request body to maxBytes.
//
// Two layers, because they fail differently:
//
//   - A client that DECLARES an oversized body (Content-Length) is refused
//     immediately with 413 and a message naming the limit. That is the honest
//     answer, and it costs nothing — no bytes are read and no handler runs.
//   - Anything that gets past that (chunked encoding, a lying or absent
//     Content-Length) is capped by http.MaxBytesReader, so the decoder fails past
//     the cap instead of buffering the whole payload.
//
// Only MaxBytesReader used to be applied, which meant an oversized request reached
// the handler and surfaced as a generic decode failure — the caller could not tell
// "too large" from "malformed", so a client had no way to react correctly. The FE
// upload path documents that it expects a 413 from the server; now every capped
// route actually gives it one.
//
// maxBytes <= 0 disables the cap (the middleware becomes a pass-through) rather
// than rejecting everything, so a misconfiguration cannot take a route offline.
func BodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if maxBytes <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			if r.ContentLength > maxBytes {
				helpers.WriteJSON(w, http.StatusRequestEntityTooLarge, helpers.Envolope{
					"msg":            "request body too large",
					"max_bytes":      maxBytes,
					"content_length": r.ContentLength,
				})
				return
			}
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}
