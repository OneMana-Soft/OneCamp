//go:build integration
// +build integration

package integration_test

// Someone outside a conversation is refused every route of it, as if it
// weren't there. Priya isn't in Maya, Jonas and Sam's group: she can't see who
// is in it or what it could be shared to, add anyone to it, hide, rename or
// leave it, make a private channel of it, read or move its read marker, read
// who she may mention in it, or list or set its pins, and the group is as it
// was. Maya, in it, sees who is.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestSomeoneOutsideAConversationIsRefusedEveryRoute$' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	dmAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	conversationController "github.com/akashc777/OneCamp/controllers/Conversation"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestSomeoneOutsideAConversationIsRefusedEveryRoute(t *testing.T) {
	ctx := context.Background()
	w := newIndexWorld(t, "maya", "jonas", "sam", "priya")
	people := map[string]*userModels.UserInfo{}
	for _, n := range []string{"maya", "jonas", "sam", "priya"} {
		people[n] = w.info(t, n)
	}
	run := func(who string, h http.HandlerFunc, body any, group string) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rc := chi.NewRouteContext()
		rc.URLParams.Add("kind", "group")
		rc.URLParams.Add("id", group)
		c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, *people[who]), chi.RouteCtxKey, rc)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(c))
		return rec.Code, rec.Body.String()
	}
	var uids, ids []string
	for _, n := range []string{"maya", "jonas", "sam"} {
		uids, ids = append(uids, w.uids[n]), append(ids, w.people[n].String())
	}
	if code, body := run("maya", chatController.CreateGroupChat, dmAdapter.ChatInfo{Participants: uids, TextHtml: "<p>Hello all</p>"}, ""); code != http.StatusOK {
		t.Fatalf("Maya starts the group: %d %s", code, body)
	}
	group, err := helpers.GenerateGroupID(ids)
	if err != nil {
		t.Fatal(err)
	}
	before := w.members(t, group)

	for _, c := range []struct {
		what string
		h    http.HandlerFunc
		body any
	}{
		{"sees who is in it", conversationController.GetMembers, nil},
		{"sees what it could be shared to", conversationController.GetShareChoices, nil},
		{"adds herself", conversationController.AddPeople, map[string]any{"people": []string{w.people["priya"].String()}}},
		{"hides it", conversationController.SetHidden, map[string]any{"hidden": true}},
		{"renames it", conversationController.Rename, map[string]any{"name": "Priya's now"}},
		{"leaves it", conversationController.Leave, nil},
		{"makes a private channel of it", conversationController.MakePrivateChannel, map[string]any{"name": "copied", "people": []string{}}},
		{"reads its read marker", conversationController.GetReadState, nil},
		{"moves its read marker", conversationController.MarkUnread, map[string]any{"from": uuid.NewString()}},
		{"reads who she may mention in it", conversationController.GetMentionables, nil},
		{"lists its pins", conversationController.GetPins, nil},
		{"pins in it", conversationController.SetPin, map[string]any{"message_id": uuid.NewString(), "pinned": true}},
	} {
		if code, body := run("priya", c.h, c.body, group); code != http.StatusNotFound {
			t.Errorf("Priya, outside the group, %s: %d %s", c.what, code, body)
		}
	}
	sameList(t, "the group's people after her tries", w.members(t, group), before...)
	if code, body := run("maya", conversationController.GetMembers, nil, group); code != http.StatusOK {
		t.Errorf("Maya, in the group, sees who is in it: %d %s", code, body)
	}
}
