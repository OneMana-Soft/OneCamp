//go:build integration
// +build integration

package integration_test

// Leaving a group conversation, against real Postgres 12, Dgraph and Redis:
// Priya leaves the four of them and is out of its people in the graph and the
// index, can't read it or write in it, and the others read "Priya left the
// conversation."; a group of three, a DM, and a group she isn't in are each
// refused in words, and nothing changes.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAGroupConversationCanBeLeft -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	dmAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	conversationController "github.com/akashc777/OneCamp/controllers/Conversation"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestAGroupConversationCanBeLeft(t *testing.T) {
	ctx := context.Background()
	w := newIndexWorld(t, "maya", "jonas", "sam", "priya")
	people := map[string]*userModels.UserInfo{}
	for _, n := range []string{"maya", "jonas", "sam", "priya"} {
		people[n] = w.info(t, n)
	}
	run := func(who string, handler http.HandlerFunc, body any, params ...string) (int, string, json.RawMessage) {
		t.Helper()
		raw, _ := json.Marshal(body)
		c := context.WithValue(ctx, helpers.UserInfoContextKey, *people[who])
		if len(params) > 0 {
			rc := chi.NewRouteContext()
			for i := 0; i+1 < len(params); i += 2 {
				rc.URLParams.Add(params[i], params[i+1])
			}
			c = context.WithValue(c, chi.RouteCtxKey, rc)
		}
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(c))
		var answer struct {
			Msg  string          `json:"msg"`
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		return rec.Code, answer.Msg, answer.Data
	}
	start := func(who string, names ...string) string {
		t.Helper()
		var uids, ids []string
		for _, n := range names {
			uids, ids = append(uids, w.uids[n]), append(ids, w.people[n].String())
		}
		if code, msg, _ := run(who, chatController.CreateGroupChat, dmAdapter.ChatInfo{Participants: uids, TextHtml: "<p>Hello all</p>"}); code != http.StatusOK {
			t.Fatalf("starting %v: %d %s", names, code, msg)
		}
		id, err := helpers.GenerateGroupID(ids)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	leave := func(who, kind, id string) (int, string) {
		code, msg, _ := run(who, conversationController.Leave, nil, "kind", kind, "id", id)
		return code, msg
	}
	graphPeople := func(id string) []string {
		t.Helper()
		var got struct {
			G []struct {
				P []struct {
					Name string `json:"user_name"`
				} `json:"dm_participants"`
			} `json:"g"`
		}
		_ = json.Unmarshal(w.dg.Query(t, `{ g(func: eq(dm_grouping_id, "`+id+`")) { dm_participants { user_name } } }`), &got)
		var out []string
		for _, g := range got.G {
			for _, p := range g.P {
				out = append(out, p.Name)
			}
		}
		return out
	}
	lines := func(id string) []string {
		t.Helper()
		var got struct {
			G []struct {
				C []struct {
					Body string `json:"chat_body_text"`
				} `json:"dm_chats"`
			} `json:"g"`
		}
		_ = json.Unmarshal(w.dg.Query(t, `{ g(func: eq(dm_grouping_id, "`+id+`")) { dm_chats @filter(eq(chat_kind, "system")) { chat_body_text } } }`), &got)
		var out []string
		for _, g := range got.G {
			for _, c := range g.C {
				out = append(out, c.Body)
			}
		}
		return out
	}

	four := start("maya", "maya", "jonas", "sam", "priya")
	three := start("maya", "maya", "jonas", "sam")

	// Priya leaves the four.
	if code, msg := leave("priya", "group", four); code != http.StatusOK {
		t.Fatalf("Priya leaving: %d %s", code, msg)
	}
	sameList(t, "the four's people in the graph", graphPeople(four), "maya", "jonas", "sam")
	sameList(t, "the four's people in the index", w.members(t, four), "maya:member", "jonas:member", "sam:member", "priya:out")
	if _, err := conversationBusiness.Resolve(ctx, people["priya"], conversationBusiness.Ref{Kind: conversationBusiness.KindGroup, ID: four}); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Errorf("Priya reading the group she left: %v", err)
	}
	if code, msg, _ := run("priya", chatController.CreateGroupChat, dmAdapter.ChatInfo{GrpUuid: four, TextHtml: "<p>one more thing</p>"}); code != http.StatusForbidden {
		t.Errorf("Priya writing in the group she left: %d %s", code, msg)
	}
	if got := lines(four); !slicesContainSubstring(got, "priya left the conversation.") {
		t.Errorf("the four's lines %v: want Priya's leaving", got)
	}
	// The three still talk.
	if code, msg, _ := run("jonas", chatController.CreateGroupChat, dmAdapter.ChatInfo{GrpUuid: four, TextHtml: "<p>Onwards</p>"}); code != http.StatusOK {
		t.Errorf("Jonas writing after Priya left: %d %s", code, msg)
	}

	// What can't be left, each in words, and nothing changes.
	if code, msg := leave("jonas", "group", three); code != http.StatusConflict || !strings.Contains(msg, "at least three people") {
		t.Errorf("Jonas leaving a group of three: %d %q", code, msg)
	}
	sameList(t, "the three's people", graphPeople(three), "maya", "jonas", "sam")
	dm := helpers.GetGroupingId(w.people["maya"].String(), w.people["jonas"].String())
	if code, msg := leave("maya", "dm", dm); code != http.StatusBadRequest {
		t.Errorf("Maya leaving a DM: %d %q", code, msg)
	}
	if code, msg := leave("priya", "group", three); code != http.StatusNotFound {
		t.Errorf("Priya leaving a group she isn't in: %d %q", code, msg)
	}
	if code, msg := leave("priya", "group", four); code != http.StatusNotFound {
		t.Errorf("Priya leaving the four again: %d %q", code, msg)
	}
}

// slicesContainSubstring is whether any of list holds part, ignoring case.
func slicesContainSubstring(list []string, part string) bool {
	for _, s := range list {
		if strings.Contains(strings.ToLower(s), strings.ToLower(part)) {
			return true
		}
	}
	return false
}
