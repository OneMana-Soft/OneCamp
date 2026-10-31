package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
)

// A search for people and places answers a list, empty when nothing matched,
// never null. The web's request hook reads a null "data" as the whole answer,
// so the calendar's guest picker filtered an object and threw ("(r ?? []).
// filter is not a function") whenever a name or address typed matched nobody,
// and kept offering what the text before it had found.
func TestASearchForPeopleAndPlacesAnswersAListEvenWhenNothingMatches(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/user/searchUserAndChannelList", strings.NewReader(`{"search_text":"   "}`))
	r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, models.UserInfo{}))
	w := httptest.NewRecorder()
	FwdUserAndChannelList(w, r)
	var body struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || string(body.Data) != "[]" {
		t.Errorf("a search of nothing answered %d with data %s, want 200 and []", w.Code, body.Data)
	}
}
