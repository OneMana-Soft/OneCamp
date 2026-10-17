package controllers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	attachmentModels "github.com/akashc777/OneCamp/models/postgres/Attachment"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/go-chi/chi/v5"
)

// A doc shared through a guest link shows its pictures and files to that
// guest (B13), and nothing else: not another doc's, not a board's, not after
// the link stops working.
func TestAGuestSeesTheFilesOfTheDocTheirLinkOpensAndNoOthers(t *testing.T) {
	const (
		token    = "good-token"
		doc      = "6a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
		otherDoc = "0f9e8d7c-6b5a-4c3d-8e2f-1a0b9c8d7e6f"
	)
	rows := map[string]*attachmentModels.Attachment{
		"in-doc":    {ObjKey: "u1/in-doc_a.png", SrcKey: postgressStruct.ATTACHMENT_SRC_DOC, SrcValue: doc},
		"other-doc": {ObjKey: "u1/other_b.png", SrcKey: postgressStruct.ATTACHMENT_SRC_DOC, SrcValue: otherDoc},
		"board":     {ObjKey: "u1/board_c.png", SrcKey: postgressStruct.ATTACHMENT_SRC_BOARD, SrcValue: doc},
	}
	oldGrant, oldRow, oldURL := guestGrant, guestAttachmentRow, guestAttachmentURL
	t.Cleanup(func() { guestGrant, guestAttachmentRow, guestAttachmentURL = oldGrant, oldRow, oldURL })
	guestGrant = func(_ context.Context, tok, _ string) (*guestModel.GuestGrant, error) {
		switch tok {
		case token:
			return &guestModel.GuestGrant{ResourceType: guestModel.ResourceDoc, ResourceID: doc}, nil
		case "store-down":
			return nil, errors.New("postgres: connection refused")
		}
		// Revoked, expired, off, or to a doc in the trash: ValidateResourceGrant's answers.
		return nil, guestBusiness.ErrInvalidGrant
	}
	guestAttachmentRow = func(_ context.Context, obj, _ string) (*attachmentModels.Attachment, error) {
		if r, ok := rows[obj]; ok {
			return r, nil
		}
		return nil, sql.ErrNoRows
	}
	var signed []string
	guestAttachmentURL = func(_ context.Context, objKey string) (string, error) {
		signed = append(signed, objKey)
		return "https://media.example.test/" + objKey + "?X-Amz-Signature=abc", nil
	}

	get := func(tok, obj string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/guest/doc-attachment/"+tok+"/"+obj, nil)
		rc := chi.NewRouteContext()
		rc.URLParams.Add("token", tok)
		rc.URLParams.Add("obj_uuid", obj)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
		rec := httptest.NewRecorder()
		GuestDocAttachment(rec, r)
		return rec
	}

	rec := get(token, "in-doc")
	if rec.Code != http.StatusTemporaryRedirect || !strings.HasPrefix(rec.Header().Get("Location"), "https://media.example.test/u1/in-doc_a.png") {
		t.Fatalf("the doc's own picture: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("a signed address must never be kept by a cache: %q", rec.Header().Get("Cache-Control"))
	}

	// Everything refused reads the same, so probing learns nothing.
	for _, c := range []struct{ tok, obj string }{
		{token, "other-doc"},
		{token, "board"},
		{token, "missing"},
		{token, ""},
		{"revoked", "in-doc"},
	} {
		rec := get(c.tok, c.obj)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no longer available") {
			t.Errorf("%s/%s: %d %s; want the uniform not-available", c.tok, c.obj, rec.Code, rec.Body.String())
		}
	}
	// A store that didn't answer is a 503 the page retries, not a dead link.
	if rec := get("store-down", "in-doc"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a grant read that failed: %d; want 503", rec.Code)
	}
	if len(signed) != 1 {
		t.Errorf("signed %v: only the doc's own file may be", signed)
	}
}
