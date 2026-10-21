package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	attachmentModels "github.com/akashc777/OneCamp/models/postgres/Attachment"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// The demo's shared visitor can't put a page on the web, or take one off it:
// anyone could write it, under the demo's address.
func TestTheDemoVisitorPublishesNothing(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example.test")
	old := grantOfLink
	t.Cleanup(func() { grantOfLink = old })
	looked := 0
	grantOfLink = func(context.Context, uuid.UUID) (*guestModel.GuestGrant, error) { looked++; return nil, nil }
	r := chi.NewRouter()
	r.Put("/guest/links/{id}/publish", PublishGuestLink)
	r.Delete("/guest/links/{id}/publish", UnpublishGuestLink)
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/guest/links/"+uuid.NewString()+"/publish", strings.NewReader(`{}`))
		who := userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "a0000000-0000-4000-8000-000000000001"}}
		who.UserPostgresInfo.EmailID = "Visitor@demo.example.test"
		req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, who))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"demo"`) {
			t.Errorf("%s: %d %s", method, w.Code, w.Body)
		}
	}
	if looked != 0 {
		t.Errorf("the link was read %d times", looked)
	}
}

// A doc's view-only link shows its pictures through its page alone, never the
// app's guest route, so a password on it is never gone round; a comment
// link's pictures still come through the app.
func TestAViewLinksFilesComeThroughItsPage(t *testing.T) {
	const doc = "6a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	oldGrant, oldRow, oldURL := guestGrant, guestAttachmentRow, guestAttachmentURL
	t.Cleanup(func() { guestGrant, guestAttachmentRow, guestAttachmentURL = oldGrant, oldRow, oldURL })
	guestGrant = func(_ context.Context, tok, _ string) (*guestModel.GuestGrant, error) {
		capability := guestModel.CapabilityView
		if tok == "comment-token" {
			capability = guestModel.CapabilityComment
		}
		return &guestModel.GuestGrant{ResourceType: guestModel.ResourceDoc, ResourceID: doc, Capability: capability}, nil
	}
	guestAttachmentRow = func(context.Context, string, string) (*attachmentModels.Attachment, error) {
		return &attachmentModels.Attachment{ObjKey: "u1/a.png", SrcKey: postgressStruct.ATTACHMENT_SRC_DOC, SrcValue: doc}, nil
	}
	guestAttachmentURL = func(context.Context, string) (string, error) { return "https://files.example.test/a.png?sig=1", nil }
	r := chi.NewRouter()
	r.Get("/guest/doc-attachment/{token}/{obj_uuid}", GuestDocAttachment)
	for token, want := range map[string]int{"view-token": http.StatusNotFound, "comment-token": http.StatusTemporaryRedirect} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/guest/doc-attachment/"+token+"/"+uuid.NewString(), nil))
		if w.Code != want {
			t.Errorf("%s: %d, want %d", token, w.Code, want)
		}
	}
}
