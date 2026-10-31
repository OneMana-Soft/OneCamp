package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Bringing your Slack DMs is a signed-in member's own act: nobody signed out,
// and no API token or agent acting for a member, reaches any of its routes.
// A request says what's wrong with it before Slack is asked anything, in
// words the member can act on.
func TestBringingYourSlackDMsIsTheMembersOwnAct(t *testing.T) {
	bo := userModel.UserInfo{UserPostgresInfo: userModel.User{Id: uuid.New(), EmailID: "bo@acme.test"}}
	ask := func(h http.HandlerFunc, ctx context.Context, body string) (int, map[string]any) {
		t.Helper()
		rc := chi.NewRouteContext()
		rc.URLParams.Add("jobId", uuid.NewString())
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)).WithContext(context.WithValue(ctx, chi.RouteCtxKey, rc))
		rec := httptest.NewRecorder()
		h(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	signedIn := context.WithValue(context.Background(), helpers.UserInfoContextKey, bo)
	throughToken := helpers.WithActsThroughToken(signedIn)

	for name, h := range map[string]http.HandlerFunc{
		"start": HandleMyRead, "list": HandleMyReads, "notes": HandleMyReadNotes, "stop": HandleStopMyRead,
	} {
		if code, _ := ask(h, context.Background(), `{"token":"xoxp-1"}`); code != http.StatusUnauthorized {
			t.Errorf("%s, signed out: %d, want 401", name, code)
		}
		if code, out := ask(h, throughToken, `{"token":"xoxp-1"}`); code != http.StatusForbidden || out["code"] != "session_only" {
			t.Errorf("%s, through an API token: %d %v, want 403 session_only", name, code, out)
		}
	}

	for _, c := range []struct{ body, code, says string }{
		{`not json`, "", "invalid body"},
		{`{"token":"xoxp-1","since":"last week"}`, "", "since is a date: YYYY-MM-DD"},
		{`{"token":"xoxb-123-456"}`, "bad_token", "A bot token reads only the channels its app was added to, never your DMs. Use your own user token: it starts xoxp-."},
		{`{"token":"ghp_123"}`, "bad_token", "That isn't a Slack user token. Yours starts xoxp-."},
		{`{}`, "bad_token", "That isn't a Slack user token. Yours starts xoxp-."},
	} {
		code, out := ask(HandleMyRead, signedIn, c.body)
		if code != http.StatusBadRequest || out["msg"] != c.says || (c.code != "" && out["code"] != c.code) {
			t.Errorf("%s: %d %v, want 400 %s %q", c.body, code, out, c.code, c.says)
		}
	}
}
