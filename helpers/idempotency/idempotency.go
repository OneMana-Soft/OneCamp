// Package idempotency answers a write sent again under the same
// Idempotency-Key from its first answer, so a retry never does the work
// twice. It is the one mechanism for every route that takes the header: the
// public API's writes (business/PublicAPI) and the app's own routes whose
// resend would make something again (a doc import, docs W3). Each route
// answers a refusal in its own error shape (Refuse).
//
// The same key from the same caller on the same route with the same body is
// answered from the first answer for 24 hours; with another body it is
// refused. A request still being answered under its key refuses the next. A
// write that failed (a 5xx, a handler that panics) lets the key go, so it can
// be tried again rather than be refused as still on its way for the key's 24
// hours. A write that was done keeps its key even when Redis failed to store
// its answer: a retry is then refused as still on its way until the 24 hours
// end, because running it a second time is the one thing the key exists to
// stop. Without a key, without a caller, or without Redis, the write simply
// runs: an outage never takes a route down.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

// keptFor is how long an answer is kept under its key.
const keptFor = 24 * time.Hour

// MaxKey bounds the header a client sends.
const MaxKey = 200

// Refusal is why a request under a key is refused.
type Refusal int

const (
	// KeyTooLong: the key is over MaxKey characters.
	KeyTooLong Refusal = iota + 1
	// Unreadable: the body couldn't be read. The error says why; an
	// *http.MaxBytesError is a body over its route's cap.
	Unreadable
	// Reused: the key came with another body in the last 24 hours.
	Reused
	// InProgress: the key's first request is still being answered.
	InProgress
)

// Refuse answers a refused request in its route's own error shape.
type Refuse func(w http.ResponseWriter, why Refusal, err error)

// kept is an answer under its key, or a claim on the key while the first
// request is answered.
type kept struct {
	Pending     bool   `json:"pending,omitempty"`
	BodyHash    string `json:"body_hash"`
	Status      int    `json:"status,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Body        []byte `json:"body,omitempty"`
}

// storeAnswer keeps an answer under its key: a seam for tests, which can't
// make Redis refuse one write.
var storeAnswer = redisStore.SetStringWithTTL

// Middleware keys a route's writes. route names the route in the key, so one
// key on two routes is two writes.
func Middleware(route string, refuse Refuse) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
			who := caller(r)
			if key == "" || who == "" || !redisStore.IsAvailable() {
				next.ServeHTTP(w, r)
				return
			}
			if len(key) > MaxKey {
				refuse(w, KeyTooLong, nil)
				return
			}
			body, err := helpers.PeekBody(r)
			if err != nil {
				refuse(w, Unreadable, err)
				return
			}
			hash := fingerprint(r.Header.Get("Content-Type"), body)
			args := []string{who, route, key}

			pending, _ := json.Marshal(kept{Pending: true, BodyHash: hash})
			claimed, _ := redisStore.SetStringIfAbsent(r.Context(), registry.ApiIdempotency, args, string(pending), keptFor)
			if !claimed {
				raw, found, _ := redisStore.GetString(r.Context(), registry.ApiIdempotency, args)
				var k kept
				if found && json.Unmarshal([]byte(raw), &k) == nil {
					switch {
					case k.BodyHash != hash:
						refuse(w, Reused, nil)
					case k.Pending:
						refuse(w, InProgress, nil)
					default:
						if k.ContentType != "" {
							w.Header().Set("Content-Type", k.ContentType)
						}
						w.Header().Set("Idempotent-Replayed", "true")
						w.WriteHeader(k.Status)
						_, _ = w.Write(k.Body)
					}
					return
				}
				// Gone between the claim and the read: run it as a first send.
			}
			done := false
			// Deferred, so a panic lets the key go too on its way to the
			// router's recoverer.
			defer func() {
				if !done {
					_ = redisStore.Delete(context.WithoutCancel(r.Context()), registry.ApiIdempotency, args)
				}
			}()
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			if rec.status >= 500 {
				return
			}
			done = true
			answer, _ := json.Marshal(kept{BodyHash: hash, Status: rec.status, ContentType: w.Header().Get("Content-Type"), Body: rec.body.Bytes()})
			if err := storeAnswer(context.WithoutCancel(r.Context()), registry.ApiIdempotency, args, string(answer), keptFor); err != nil {
				helpers.LogWarnWithContext(r.Context(), "helpers/idempotency %s: a done write's answer wasn't kept, so its key stays claimed until it expires: %v", route, err)
			}
		})
	}
}

// caller is whose key it is: an API token's (its id, as the public API has
// always keyed), else the signed-in member's. "" is nobody.
func caller(r *http.Request) string {
	if token := helpers.GetApiTokenID(r.Context()); token != "" {
		return token
	}
	if u, ok := userModels.FromContext(r.Context()); ok && u.UserPostgresInfo.Id != uuid.Nil {
		return "member:" + u.UserPostgresInfo.Id.String()
	}
	return ""
}

// fingerprint is what makes two sends the same request: the body's hash. A
// multipart form's boundary is left out of it, since a browser writes a new
// one each time it sends the same form; the boundary never occurs in the
// form's content, so nothing else is left out.
func fingerprint(contentType string, body []byte) string {
	if media, params, err := mime.ParseMediaType(contentType); err == nil && strings.HasPrefix(media, "multipart/") && params["boundary"] != "" {
		body = bytes.ReplaceAll(body, []byte("--"+params["boundary"]), nil)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// InWords answers a refusal as the app's own routes answer one: {"msg"}, in
// words a person reads.
func InWords(w http.ResponseWriter, why Refusal, err error) {
	status, msg := http.StatusBadRequest, "The request couldn't be read. Send it again."
	switch why {
	case KeyTooLong:
		msg = fmt.Sprintf("An Idempotency-Key is at most %d characters.", MaxKey)
	case Unreadable:
		if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
			status, msg = http.StatusRequestEntityTooLarge, "That's larger than this server takes in one request."
		}
	case Reused:
		status, msg = http.StatusUnprocessableEntity, "This request's key was used for something else. Start it again."
	case InProgress:
		status, msg = http.StatusConflict, "This is already on its way. Wait a moment, then look again."
	}
	helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
}

// recorder keeps a copy of what a handler wrote.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	wrote  bool
}

func (r *recorder) WriteHeader(status int) {
	if !r.wrote {
		r.status = status
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}
