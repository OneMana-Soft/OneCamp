package helpers

import (
	"bytes"
	"io"
	"net/http"
)

// PeekBody is a request's body, read and put back for the handler: how a step
// before the handler reads it (an app's Reach from a write's body, an
// Idempotency-Key's fingerprint) without taking it from the handler.
func PeekBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, err
}
