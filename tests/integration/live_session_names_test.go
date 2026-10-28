//go:build integration
// +build integration

package integration_test

// The answers the collaboration service gets when a member's tab opens a live
// doc or board name the member: the service puts that name on their presence,
// since a session token carries none, so it is what everyone else there sees
// (guests on a call's stage too). Never an address: a set display name first,
// then the name the graph holds, else none.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestTheLiveSessionChecksNameWhoOpens' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	"github.com/akashc777/OneCamp/helpers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/router"
)

func TestTheLiveSessionChecksNameWhoOpens(t *testing.T) {
	env, dg := liveSessionStores(t)
	ctx := context.Background()
	// "set" has a display name; "graph" only the graph's name; "nameless" neither, only an address.
	people := livePeople(t, env, "set", "graph", "nameless")
	if _, err := env.PG.Exec(`UPDATE users SET display_name = 'Sam Rivera' WHERE id = $1`, people["set"]); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"graph", "nameless"} {
		if _, err := env.PG.Exec(`UPDATE users SET display_name = NULL WHERE id = $1`, people[who]); err != nil {
			t.Fatal(err)
		}
	}
	boards := map[string]string{"set": uuid.NewString(), "graph": uuid.NewString(), "nameless": uuid.NewString()}
	board := func(who string) map[string]any {
		return map[string]any{"uid": "_:b" + who, "dgraph.type": "Board", "board_uuid": boards[who], "board_title": "Retro " + who, "board_private": true,
			"board_deleted_at": "0001-01-01T00:00:00Z", "board_created_by": map[string]any{"uid": "_:" + who}}
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:set", "dgraph.type": "User", "user_uuid": people["set"].String(), "user_name": "sam"},
		{"uid": "_:graph", "dgraph.type": "User", "user_uuid": people["graph"].String(), "user_name": "Maya Chen"},
		{"uid": "_:nameless", "dgraph.type": "User", "user_uuid": people["nameless"].String()},
		board("set"), board("graph"), board("nameless"),
	})
	priv := true
	routes := router.Routes()
	session := customMiddleware.VerifyDocColabAuth(http.HandlerFunc(docController.DocCollabSession))

	for who, want := range map[string]string{"set": "Sam Rivera", "graph": "Maya Chen", "nameless": ""} {
		token, err := helpers.SignSessionToken(people[who].String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		doc, err := docBusiness.CreateDocInTree(ctx, &dgraphStruct.DgraphUser{Uid: uids[who], Uuid: people[who].String()}, docBusiness.NewDoc{Title: "Plan " + who, Private: &priv}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			path, body string
			serve      http.Handler
		}{
			{"/boardColab/authorize", `{"board_uuid":"` + boards[who] + `"}`, routes},
			{"/docColab/session", `{"doc_uuid":"` + doc.Uuid + `"}`, session},
		} {
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			c.serve.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("%s opening their own on %s: answered %d (%s)", who, c.path, rec.Code, strings.TrimSpace(rec.Body.String()))
				continue
			}
			if strings.Contains(rec.Body.String(), "@") {
				t.Errorf("%s on %s: the answer holds an address: %s", who, c.path, rec.Body.String())
			}
			var out struct {
				Name *string `json:"name"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			got := ""
			if out.Name != nil {
				got = *out.Name
			}
			if got != want || (want != "" && out.Name == nil) {
				t.Errorf("%s on %s: named %q, want %q (%s)", who, c.path, got, want, rec.Body.String())
			}
		}
	}
}
