//go:build integration
// +build integration

package integration_test

// A person's calls over HTTP for the docs world's tests: any route's handler
// with its path's {names}, its query, and a guest link made as them.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/go-chi/chi/v5"

	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// callAt runs a route's handler as a person (an admin acting themselves when
// admin is set), with body as its JSON and params as its path's {names}.
func (w *docsWorld) callAt(handler http.HandlerFunc, name string, admin bool, body string, params ...string) *httptest.ResponseRecorder {
	info := userModels.UserInfo{}
	info.UserPostgresInfo.Id = w.people[name]
	info.UserPostgresInfo.EmailID = w.people[name].String() + "@example.test"
	info.UserPostgresInfo.IsAdmin = admin
	info.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: w.uids[name], Uuid: w.people[name].String()}
	route := chi.NewRouteContext()
	for i := 0; i+1 < len(params); i += 2 {
		route.URLParams.Add(params[i], params[i+1])
	}
	ctx := context.WithValue(context.WithValue(context.Background(), helpers.UserInfoContextKey, info), chi.RouteCtxKey, route)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// callQuery runs a route's handler as a person with query as its URL's.
func (w *docsWorld) callQuery(handler http.HandlerFunc, name, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/"+query, nil).WithContext(w.as(name, ""))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// guestLink makes an external link to a board or a doc as a person, and
// answers the response.
func (w *docsWorld) guestLink(name, kind, id string) *httptest.ResponseRecorder {
	return w.callAt(guestController.CreateResourceGuestLink, name, false,
		`{"resource_type":"`+kind+`","resource_id":"`+id+`","capability":"view","ttl_hours":24}`)
}
