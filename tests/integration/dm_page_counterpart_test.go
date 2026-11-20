//go:build integration
// +build integration

package integration_test

// V-1201: a DM page answers for its counterpart whatever page it is and
// however their uuid is spelled. N1: the page newer from a message, with
// nobody (or not a uuid) as the counterpart, answered 200 and an empty page
// where its three siblings answer 400. N2: the pages built the DM's grouping
// from the uuid as sent, and uuid.Parse also takes upper case, {...} and
// urn:uuid: spellings: bea's uuid in upper case opened an empty DM that isn't
// theirs, and the latest page marked it read.
//
// Run: PKGS=./tests/integration/ integ_rel.sh <wt> <log> 'TestADMPage'

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

func TestADMPageNewerFromAMessageAnswersForNobody(t *testing.T) {
	w := newOpenWorld(t)
	at := w.dmChats[0]
	ghost := uuid.NewString()
	if code, _ := w.get("ada", chatController.GetNewChatsIncludingChat, "user_uuid", w.people["bea"].String(), "chat_uuid", at); code != http.StatusOK {
		t.Fatalf("ada's page with bea newer from her message: %d, want 200", code)
	}
	for _, other := range []string{ghost, "not-a-uuid"} {
		if code, _ := w.get("ada", chatController.GetNewChatsIncludingChat, "user_uuid", other, "chat_uuid", at); code != http.StatusBadRequest {
			t.Errorf("ada's page with %q newer from a message: %d, want 400", other, code)
		}
		if w.marked("ada", helpers.GetGroupingId(w.people["ada"].String(), other)) {
			t.Errorf("ada's page with %q newer from a message marked a DM read", other)
		}
	}
}

func TestADMPageReadsItsCounterpartHoweverSpelled(t *testing.T) {
	w := newOpenWorld(t)
	upper := strings.ToUpper(w.people["bea"].String())
	code, page := w.get("ada", chatController.GetLatestChats, "user_uuid", upper)
	if code != http.StatusBadRequest && (code != http.StatusOK || !slices.Equal(page.Chats, rev(w.dmChats))) {
		t.Errorf("ada's latest page with bea's uuid in upper case: %d %v, want 200 %v (or 400)", code, page.Chats, rev(w.dmChats))
	}
	if w.marked("ada", helpers.GetGroupingId(w.people["ada"].String(), upper)) {
		t.Errorf("ada's latest page with bea's uuid in upper case marked a DM that isn't theirs read")
	}
}

// A DM page with nobody as its counterpart refuses (its own 400, no "err")
// rather than failing: a 400 carrying an "err" is a read that failed (logged
// as an error), which is what the user read's not-found would become if the
// page stopped naming it ErrNoSuchPerson.
func TestADMPageForNobodyIsARefusalNotAFailure(t *testing.T) {
	w := newOpenWorld(t)
	ghost := uuid.NewString()
	pages := []struct {
		name    string
		handler http.HandlerFunc
		params  []string
	}{
		{"latest", chatController.GetLatestChats, []string{"user_uuid", ghost}},
		{"older", chatController.GetOldChats, []string{"user_uuid", ghost, "time_stamp", w.seconds(2)}},
		{"newer", chatController.GetNewChats, []string{"user_uuid", ghost, "time_stamp", w.seconds(0)}},
		{"newer from a message", chatController.GetNewChatsIncludingChat, []string{"user_uuid", ghost, "chat_uuid", w.dmChats[0]}},
	}
	for _, p := range pages {
		code, body := w.said("ada", p.handler, p.params...)
		if _, failed := body["err"]; code != http.StatusBadRequest || failed {
			t.Errorf("ada's %s page with nobody: %d %v, want a 400 refusal with no err", p.name, code, body)
		}
	}
}

// said is handler's answer to name with the URL params given, as get sends
// it: its status and its JSON body.
func (w *openWorld) said(name string, handler http.HandlerFunc, params ...string) (int, map[string]any) {
	var body map[string]any
	code, _ := w.get(name, func(rw http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		handler(rec, r)
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		rw.WriteHeader(rec.Code)
		_, _ = rw.Write(rec.Body.Bytes())
	}, params...)
	return code, body
}
