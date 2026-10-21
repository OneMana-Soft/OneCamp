package middleware

import (
	"net/http"

	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
	"golang.org/x/net/context"
)

const REQ_ID_KEY = "X-Request-ID"

func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get the request ID from the header, or generate a new one if it doesn't exist.
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}

		// Add the request ID to the context.
		ctx := context.WithValue(r.Context(), REQ_ID_KEY, requestID)
		// A place for the request's task filter, so its reader's restricted
		// tasks are worked out once however many reads it makes
		// (models/taskread).
		ctx = taskread.Install(ctx)

		// Call the next handler in the chain.
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
