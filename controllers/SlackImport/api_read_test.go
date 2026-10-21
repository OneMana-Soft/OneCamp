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
	"github.com/google/uuid"
)

// A read through Slack's API says what's wrong with its request before any job
// is made: the workspace's name, a token that is Slack's, a date for since. A
// member who isn't an admin is refused.
func TestAnAPIReadSaysWhatsWrongWithItsRequest(t *testing.T) {
	ask := func(admin bool, body string) (int, string) {
		who := userModel.UserInfo{UserPostgresInfo: userModel.User{IsAdmin: admin, Id: uuid.New()}}
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)).
			WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, who))
		rec := httptest.NewRecorder()
		HandleAPIRead(rec, req)
		var out struct {
			Msg string `json:"msg"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Msg
	}
	for _, tc := range []struct{ body, says string }{
		{`not json`, "invalid body"},
		{`{"token":"xoxp-1"}`, "slack_workspace_name is required"},
		{`{"slack_workspace_name":"Acme","token":"ghp_123"}`, "That isn't a Slack token. A person's token starts xoxp-, an app's xoxb-."},
		{`{"slack_workspace_name":"Acme","token":""}`, "That isn't a Slack token. A person's token starts xoxp-, an app's xoxb-."},
		{`{"slack_workspace_name":"Acme","token":"xoxp-1","max_messages":-1}`, "max_messages can't be negative"},
		{`{"slack_workspace_name":"Acme","token":"xoxb-1","since":"last week"}`, "since is a date: YYYY-MM-DD"},
	} {
		if code, says := ask(true, tc.body); code != http.StatusBadRequest || says != tc.says {
			t.Errorf("%s: %d %q, want 400 %q", tc.body, code, says, tc.says)
		}
	}
	if code, _ := ask(false, `{"slack_workspace_name":"Acme","token":"xoxp-1"}`); code != http.StatusForbidden {
		t.Errorf("a member who isn't an admin: %d, want 403", code)
	}
}
