//go:build integration
// +build integration

package integration_test

// The four routes the collaboration service asks before it lets a socket into
// a doc or a board are public by their chain. The service calls them with the
// token the browser handed it: a member's access token on /docColab/authorize
// and /boardColab/authorize (VerifyDocColabAuth, then the doc's or the
// board's own rule), a guest's collab token on the guestAuthorize pair
// (AuthorizeGuestCollab, against the token's grant). Nothing else stands in
// front of them, so each must refuse a token this server didn't sign, one
// that has run out, one of the other kind, and one for a doc or board it
// doesn't open, and still let the right one in. Driven through the real
// router; router/liveSessionAuth_test.go has a caller with no token at all,
// and the LiveKit webhook.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestTheLiveSessionChecksTakeOnlyTheirOwnTokens' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/akashc777/OneCamp/router"
)

func TestTheLiveSessionChecksTakeOnlyTheirOwnTokens(t *testing.T) {
	env, dg := liveSessionStores(t)
	ctx := context.Background()
	people := livePeople(t, env, "owner", "maya")
	board, otherBoard := uuid.NewString(), uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": people["owner"].String(), "user_name": "owner"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": people["maya"].String(), "user_name": "maya"},
		{"uid": "_:b1", "dgraph.type": "Board", "board_uuid": board, "board_title": "Launch retro", "board_private": true,
			"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": "_:owner"}},
		{"uid": "_:b2", "dgraph.type": "Board", "board_uuid": otherBoard, "board_title": "Hiring plan", "board_private": true,
			"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": "_:owner"}},
	})
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	by := &dgraphStruct.DgraphUser{Uid: uids["owner"], Uuid: people["owner"].String(), UserName: "owner"}
	priv := true
	newDoc := func(title string) string {
		t.Helper()
		d, err := docBusiness.CreateDocInTree(ctx, by, docBusiness.NewDoc{Title: title, Private: &priv}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return d.Uuid
	}
	plan, otherDoc := newDoc("Launch plan"), newDoc("Hiring notes")

	// liveLink is a live link the owner made ("comment": a doc's view link is
	// a page, never live) and the collab token a guest's browser gets for it.
	liveLink := func(kind, id, capability string) (uuid.UUID, string) {
		t.Helper()
		g, err := guestBusiness.CreateResourceGrant(ctx, people["owner"], kind, id, capability, time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		grant, err := guestModel.GetByID(ctx, g.GrantID)
		if err != nil {
			t.Fatal(err)
		}
		token, err := guestBusiness.IssueGuestCollabToken(ctx, grant, "Ana")
		if err != nil {
			t.Fatal(err)
		}
		return grant.Id, token
	}
	docLink, docGuest := liveLink("doc", plan, "comment")
	boardLink, boardGuest := liveLink("board", board, "view")

	signedWith := func(secret string, claims jwt.MapClaims) string {
		t.Helper()
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	session := func(who, typ string, exp time.Time) string {
		t.Helper()
		token, err := helpers.SignSessionToken(people[who].String(), typ, exp.Unix())
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	// guestClaims are a guest collab token's, as IssueGuestCollabToken writes them.
	guestClaims := func(grant uuid.UUID, kind, id string, exp time.Time) jwt.MapClaims {
		return jwt.MapClaims{"sub": "guest-" + grant.String(), "guest": true, "gid": grant.String(), "rt": kind, "rid": id, "exp": exp.Unix()}
	}
	const notOurs = "a-secret-this-server-never-had"
	soon, past := time.Now().Add(10*time.Minute), time.Now().Add(-time.Minute)
	owner, maya := session("owner", helpers.TokenTypeAccess, soon), session("maya", helpers.TokenTypeAccess, soon)
	forged := signedWith(notOurs, jwt.MapClaims{"sub": people["owner"].String(), "typ": helpers.TokenTypeAccess, "exp": soon.Unix()})
	expired := session("owner", helpers.TokenTypeAccess, past)
	refresh := session("owner", helpers.TokenTypeRefresh, soon)
	secret := os.Getenv("JWT_SECRET")

	routes := router.Routes()
	docBody := func(id string) string { return `{"doc_uuid":"` + id + `"}` }
	boardBody := func(id string) string { return `{"board_uuid":"` + id + `"}` }
	for _, c := range []struct {
		name, path, token, body string
		want                    int
		canEdit                 bool
	}{
		{"the owner opening their doc", "/docColab/authorize", owner, docBody(plan), http.StatusOK, true},
		{"a token this server didn't sign", "/docColab/authorize", forged, docBody(plan), http.StatusUnauthorized, false},
		{"a token that has run out", "/docColab/authorize", expired, docBody(plan), http.StatusUnauthorized, false},
		{"a refresh token", "/docColab/authorize", refresh, docBody(plan), http.StatusUnauthorized, false},
		{"a guest's collab token", "/docColab/authorize", docGuest, docBody(plan), http.StatusUnauthorized, false},
		{"a member the doc isn't shared with", "/docColab/authorize", maya, docBody(plan), http.StatusForbidden, false},

		{"the owner opening their board", "/boardColab/authorize", owner, boardBody(board), http.StatusOK, true},
		{"a token this server didn't sign", "/boardColab/authorize", forged, boardBody(board), http.StatusUnauthorized, false},
		{"a token that has run out", "/boardColab/authorize", expired, boardBody(board), http.StatusUnauthorized, false},
		{"a refresh token", "/boardColab/authorize", refresh, boardBody(board), http.StatusUnauthorized, false},
		{"a guest's collab token", "/boardColab/authorize", boardGuest, boardBody(board), http.StatusUnauthorized, false},
		{"a member the board isn't shared with", "/boardColab/authorize", maya, boardBody(board), http.StatusForbidden, false},

		{"the guest opening the doc their link is for", "/docColab/guestAuthorize", docGuest, docBody(plan), http.StatusOK, false},
		{"a guest token this server didn't sign", "/docColab/guestAuthorize", signedWith(notOurs, guestClaims(docLink, "doc", plan, soon)), docBody(plan), http.StatusUnauthorized, false},
		{"a guest token that has run out", "/docColab/guestAuthorize", signedWith(secret, guestClaims(docLink, "doc", plan, past)), docBody(plan), http.StatusUnauthorized, false},
		{"a member's access token", "/docColab/guestAuthorize", owner, docBody(plan), http.StatusUnauthorized, false},
		{"the guest's token for another doc", "/docColab/guestAuthorize", docGuest, docBody(otherDoc), http.StatusUnauthorized, false},
		{"a board link's token", "/docColab/guestAuthorize", boardGuest, docBody(board), http.StatusUnauthorized, false},

		{"the guest opening the board their link is for", "/boardColab/guestAuthorize", boardGuest, boardBody(board), http.StatusOK, false},
		{"a guest token this server didn't sign", "/boardColab/guestAuthorize", signedWith(notOurs, guestClaims(boardLink, "board", board, soon)), boardBody(board), http.StatusUnauthorized, false},
		{"a guest token that has run out", "/boardColab/guestAuthorize", signedWith(secret, guestClaims(boardLink, "board", board, past)), boardBody(board), http.StatusUnauthorized, false},
		{"a member's access token", "/boardColab/guestAuthorize", owner, boardBody(board), http.StatusUnauthorized, false},
		{"the guest's token for another board", "/boardColab/guestAuthorize", boardGuest, boardBody(otherBoard), http.StatusUnauthorized, false},
		{"a doc link's token", "/boardColab/guestAuthorize", docGuest, boardBody(plan), http.StatusUnauthorized, false},
	} {
		req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.token)
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s on %s: answered %d, want %d (%s)", c.name, c.path, rec.Code, c.want, strings.TrimSpace(rec.Body.String()))
			continue
		}
		if c.want != http.StatusOK {
			continue
		}
		var out struct {
			CanEdit bool `json:"canEdit"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.CanEdit != c.canEdit {
			t.Errorf("%s on %s: canEdit %v (%v), want %v", c.name, c.path, out.CanEdit, err, c.canEdit)
		}
	}
}
