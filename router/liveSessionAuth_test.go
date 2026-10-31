package router

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
)

// Five routes are public by their chain, no session in front of them, and
// check their caller's credential themselves:
//   - /docColab/authorize and /boardColab/authorize: the collaboration
//     service asks with the member's own access token (VerifyDocColabAuth);
//   - /docColab/guestAuthorize and /boardColab/guestAuthorize: it asks with a
//     guest's collab token, verified against its grant (AuthorizeGuestCollab);
//   - /livekit/webhook: LiveKit's own server, its event signed with this
//     server's LiveKit API secret (webhook.Receive).
// These drive them as the router mounts them. What a token lets in, and what
// a forged, expired or misdirected one doesn't, is in
// tests/integration/live_session_tokens_test.go.

// routerMux is the router, built once for the package's tests.
var routerMux = sync.OnceValue(routes)

// quietLogs gives the package's log lines somewhere to go: a refusal logs one.
func quietLogs() {
	if helpers.MessageLogs == nil {
		d := log.New(io.Discard, "", 0)
		helpers.MessageLogs = &helpers.Message{InfoLog: d, ErrorLog: d}
	}
}

const liveKitKey, liveKitSecret = "lk-webhook-key", "lk-webhook-secret-of-thirty-two-bytes"

// withLiveKitKeys gives the server LiveKit API keys, as its configuration
// does, for the test's length.
func withLiveKitKeys(t *testing.T) {
	t.Helper()
	was := livekitInit.LiveKitService.Config
	livekitInit.LiveKitService.Config = &livekitInit.LiveKitConfigStruct{ApiKey: liveKitKey, ApiSecret: liveKitSecret}
	t.Cleanup(func() { livekitInit.LiveKitService.Config = was })
}

func TestTheLiveSessionRoutesRefuseACallerWithNoToken(t *testing.T) {
	quietLogs()
	withLiveKitKeys(t)
	t.Setenv("JWT_SECRET", "live-session-routes")
	// Guest links on, so a guest route's refusal is the token's, not the policy's.
	t.Setenv("GUEST_ACCESS_ENABLED", "true")
	id := `"00000000-0000-4000-8000-000000000000"`
	for _, c := range []struct{ path, body string }{
		{"/docColab/authorize", `{"doc_uuid":` + id + `}`},
		{"/boardColab/authorize", `{"board_uuid":` + id + `}`},
		{"/docColab/guestAuthorize", `{"doc_uuid":` + id + `}`},
		{"/boardColab/guestAuthorize", `{"board_uuid":` + id + `}`},
		{"/livekit/webhook", `{"event":"room_started","room":{"name":"standup"}}`},
	} {
		rec := httptest.NewRecorder()
		routerMux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body)))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s with no token answered %d, want 401", c.path, rec.Code)
		}
	}
}

func TestTheLiveKitWebhookTakesOnlyEventsLiveKitSigned(t *testing.T) {
	quietLogs()
	withLiveKitKeys(t)
	const key, secret = liveKitKey, liveKitSecret

	// An event the server acknowledges and does nothing with, so a 200 is the
	// signature let through and nothing else.
	event := []byte(`{"event":"room_started","room":{"name":"standup"}}`)
	signed := func(key, secret string, body []byte) string {
		t.Helper()
		sum := sha256.Sum256(body)
		token, err := auth.NewAccessToken(key, secret).
			SetValidFor(time.Minute).
			SetSha256(base64.StdEncoding.EncodeToString(sum[:])).
			ToJWT()
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	for _, c := range []struct {
		name, authorization string
		body                []byte
		want                int
	}{
		{"unsigned", "", event, http.StatusUnauthorized},
		{"signed with another secret", signed(key, "another-secret-of-thirty-two-bytes", event), event, http.StatusUnauthorized},
		{"signed under another key", signed("another-key", secret, event), event, http.StatusUnauthorized},
		{"a body other than the one signed", signed(key, secret, event), []byte(`{"event":"room_finished","room":{"name":"standup"}}`), http.StatusUnauthorized},
		{"signed by LiveKit", signed(key, secret, event), event, http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodPost, "/livekit/webhook", bytes.NewReader(c.body))
		if c.authorization != "" {
			req.Header.Set("Authorization", c.authorization)
		}
		rec := httptest.NewRecorder()
		routerMux().ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("an event %s answered %d, want %d", c.name, rec.Code, c.want)
		}
	}
}
