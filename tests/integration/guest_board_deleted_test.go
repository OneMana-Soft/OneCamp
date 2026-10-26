//go:build integration
// +build integration

package integration_test

// A deleted board opens to nobody, and that includes whoever holds one of its
// guest links: the link's own exchange, the live canvas's authorize and the
// collaboration service's read of the board all refuse it once it's deleted,
// and nobody, its owner included, can make a new link to it. A live board's
// links are its creator's and editors' to make: a member a private board is
// hidden from is told it isn't there, and one who may only read it is refused.
//
// Run: go test -tags=integration ./tests/integration/ -run TestADeletedBoardOpensToNoGuestLink -v

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	boardController "github.com/akashc777/OneCamp/controllers/Board"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestADeletedBoardOpensToNoGuestLink(t *testing.T) {
	env, dg := liveSessionStores(t)
	recordLiveOps(t)
	ctx := context.Background()
	people := livePeople(t, env, "owner", "maya", "reader")
	board, private := uuid.NewString(), uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": people["owner"].String(), "user_name": "owner"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": people["maya"].String(), "user_name": "maya"},
		{"uid": "_:reader", "dgraph.type": "User", "user_uuid": people["reader"].String(), "user_name": "reader"},
		{"uid": "_:b", "dgraph.type": "Board", "board_uuid": board, "board_title": "Launch retro", "board_private": false,
			"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": "_:owner"}},
		{"uid": "_:p", "dgraph.type": "Board", "board_uuid": private, "board_title": "Pay bands", "board_private": true,
			"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": "_:owner"},
			"board_reading_users": []map[string]any{{"uid": "_:reader"}}},
	})
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	userOf := func(name string) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = people[name]
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids[name], Uuid: people[name].String(), UserName: name}
		return u
	}
	owner := userOf("owner")
	newLink := func(who userModels.UserInfo, id string) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/guest/links",
			strings.NewReader(`{"resource_type":"board","resource_id":"`+id+`","capability":"view","ttl_hours":1}`))
		w := httptest.NewRecorder()
		guestController.CreateResourceGuestLink(w, r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, who)))
		return w.Code
	}
	// A private board's links: a member it's hidden from is told it isn't
	// there, never that it exists; one it's shared with to read is refused.
	if code := newLink(userOf("maya"), private); code != http.StatusNotFound {
		t.Errorf("a link to a private board hidden from her answers %d", code)
	}
	if code := newLink(userOf("reader"), private); code != http.StatusForbidden {
		t.Errorf("a link to a private board shared with him to read answers %d", code)
	}
	collab := chi.NewRouter()
	collab.Get("/boardColab/getBoard/{board_uuid}", boardController.GetBoardForCollab)
	collabRead := func() int {
		w := httptest.NewRecorder()
		collab.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boardColab/getBoard/"+board, nil))
		return w.Code
	}

	link, err := guestBusiness.CreateResourceGrant(ctx, people["owner"], "board", board, "view", time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := guestModel.GetByID(ctx, link.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := guestBusiness.IssueGuestCollabToken(ctx, grant, "Ana")
	if err != nil {
		t.Fatal(err)
	}
	// Live, the board opens through its link, and its owner may make another.
	if _, err := guestBusiness.ValidateCollabGrant(ctx, link.Token); err != nil {
		t.Fatalf("before: the link is refused: %v", err)
	}
	if _, err := guestBusiness.AuthorizeGuestCollab(ctx, token, "board", board); err != nil {
		t.Fatalf("before: the guest's canvas is refused: %v", err)
	}
	if code := collabRead(); code != http.StatusOK {
		t.Fatalf("before: the collaboration service's read answers %d", code)
	}
	if code := newLink(owner, board); code != http.StatusOK {
		t.Fatalf("before: the owner's new link answers %d", code)
	}

	if err := boardBusiness.DeleteBoard(ctx, board); err != nil {
		t.Fatal(err)
	}
	if _, err := guestBusiness.ValidateCollabGrant(ctx, link.Token); err == nil {
		t.Error("the deleted board still opens through its link")
	}
	if _, err := guestBusiness.AuthorizeGuestCollab(ctx, token, "board", board); err == nil {
		t.Error("the guest's live canvas reconnects to the deleted board")
	}
	if code := collabRead(); code != http.StatusNotFound {
		t.Errorf("the collaboration service reads the deleted board: %d", code)
	}
	if code := newLink(owner, board); code != http.StatusNotFound {
		t.Errorf("a new link to the deleted board answers %d", code)
	}
}
