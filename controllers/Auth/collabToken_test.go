package controllers

// The collaboration token is the session this server signed in, read the way
// every middleware reads it. It used to be the first cookie named
// Authorization, which, while browsers still carry the cookies all of a zone's
// workspaces wrote on the zone, can be another workspace's session: handed to
// the editor, refused by the collaboration server, and sent to the wrong host.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
)

func TestTheCollabTokenIsThisServersOwnSession(t *testing.T) {
	t.Setenv("FE_DOMAIN", "onemana.dev")
	t.Setenv("FE_HOST_DOMAIN", "acme.onemana.dev")
	t.Setenv("BACKEND_DOMAIN", "acme-api.onemana.dev")
	t.Setenv("COOKIE_SECURE", "")
	sign := func(key string) string {
		t.Helper()
		t.Setenv("JWT_SECRET", key)
		s, err := helpers.SignSessionToken(uuid.NewString(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	theirs := sign("beta-workspace-secret-0123456789abcdef")
	ours := sign("acme-workspace-secret-0123456789abcdef")

	ask := func(cookies ...*http.Cookie) (int, string) {
		r := httptest.NewRequest(http.MethodGet, "/auth/token", nil)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		GetCollabToken(rec, r)
		var body struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.Token
	}

	if code, tok := ask(&http.Cookie{Name: "Authorization", Value: theirs}, &http.Cookie{Name: "__Host-Authorization", Value: ours}); code != http.StatusOK || tok != ours {
		t.Errorf("with the host's own cookie and another workspace's: %d, ours=%v", code, tok == ours)
	}
	if code, tok := ask(&http.Cookie{Name: "Authorization", Value: theirs}, &http.Cookie{Name: "Authorization", Value: ours}); code != http.StatusOK || tok != ours {
		t.Errorf("with two old cookies, another workspace's first: %d, ours=%v", code, tok == ours)
	}
	if code, tok := ask(&http.Cookie{Name: "Authorization", Value: theirs}); code != http.StatusUnauthorized || tok != "" {
		t.Errorf("with only another workspace's cookie: %d, handed a token=%v", code, tok != "")
	}
}
