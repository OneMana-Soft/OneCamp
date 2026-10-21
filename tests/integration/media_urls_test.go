//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
)

// A profile picture and, when asked, a place's file answer with a redirect the
// page loads directly and the browser may keep: the picture is fetched once,
// where the web app asked getFile for every avatar and re-signed it every four
// minutes. Only a profile picture is an avatar, and only someone signed in
// gets one.
func TestPicturesAreRedirectsTheBrowserKeeps(t *testing.T) {
	p := setUpPlacesForFiles(t)
	avatar, private := uuid.NewString(), uuid.NewString()
	for id, row := range map[string][2]string{avatar: {"public", ""}, private: {"channel", p.channel}} {
		if _, err := p.env.PG.Exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, $3, $4, $5)`,
			id, p.people["mia"].String()+"/"+id+"_face.png", row[0], row[1], p.people["mia"]); err != nil {
			t.Fatal(err)
		}
	}
	get := func(handler http.HandlerFunc, who string, params map[string]string, query string) *httptest.ResponseRecorder {
		t.Helper()
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		ctx := context.WithValue(context.Background(), chi.RouteCtxKey, rctx)
		if who != "" {
			ctx = context.WithValue(ctx, helpers.UserInfoContextKey, p.as(who))
		}
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "/"+query, nil).WithContext(ctx))
		return rec
	}

	rec := get(userController.ServeAvatar, "oli", map[string]string{"obj_uuid": avatar}, "")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") == "" {
		t.Fatalf("an avatar: %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "private, max-age=3000" {
		t.Errorf("the browser may keep an avatar's redirect, privately: %q", cc)
	}
	if rec := get(userController.ServeAvatar, "oli", map[string]string{"obj_uuid": private}, ""); rec.Code != http.StatusNotFound {
		t.Errorf("a channel's file is not an avatar: %d", rec.Code)
	}
	if rec := get(userController.ServeAvatar, "oli", map[string]string{"obj_uuid": "nope"}, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("not an id: %d", rec.Code)
	}
	if rec := get(userController.ServeAvatar, "", map[string]string{"obj_uuid": avatar}, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("nobody signed in: %d", rec.Code)
	}

	// A place's file is JSON for the app, and a redirect when asked.
	if rec := get(userController.GetChannelFile, "mia", map[string]string{"channel_uuid": p.channel, "obj_uuid": private}, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"url"`) {
		t.Errorf("a channel's file as JSON: %d %s", rec.Code, rec.Body.String())
	}
	rec = get(userController.GetChannelFile, "mia", map[string]string{"channel_uuid": p.channel, "obj_uuid": private}, "?redirect=1")
	if rec.Code != http.StatusFound || rec.Header().Get("Cache-Control") != "private, max-age=240" {
		t.Errorf("a channel's file as a redirect, kept only minutes (a member who leaves loses it soon): %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec := get(userController.GetChannelFile, "oli", map[string]string{"channel_uuid": p.channel, "obj_uuid": private}, "?redirect=1"); rec.Code != http.StatusForbidden {
		t.Errorf("someone not in the channel gets no redirect: %d", rec.Code)
	}
}
