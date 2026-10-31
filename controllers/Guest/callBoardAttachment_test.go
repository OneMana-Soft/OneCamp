package controllers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	attachmentModels "github.com/akashc777/OneCamp/models/postgres/Attachment"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/go-chi/chi/v5"
)

// A call's guest sees the pictures of the board on their call's stage while
// the call lets them open it (their call's board token, re-checked on every
// read), and no other board's or doc's.
func TestACallsGuestSeesThePicturesOfTheBoardOnItsStage(t *testing.T) {
	const (
		board = "6a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
		other = "0f9e8d7c-6b5a-4c3d-8e2f-1a0b9c8d7e6f"
	)
	rows := map[string]*attachmentModels.Attachment{
		"on-board":    {ObjKey: "u1/on_a.png", SrcKey: postgressStruct.ATTACHMENT_SRC_BOARD, SrcValue: board},
		"other-board": {ObjKey: "u1/other_b.png", SrcKey: postgressStruct.ATTACHMENT_SRC_BOARD, SrcValue: other},
		"in-doc":      {ObjKey: "u1/doc_c.png", SrcKey: postgressStruct.ATTACHMENT_SRC_DOC, SrcValue: board},
	}
	oldGrant, oldCall, oldRow, oldURL := guestGrant, guestCallBoard, guestAttachmentRow, guestAttachmentURL
	t.Cleanup(func() {
		guestGrant, guestCallBoard, guestAttachmentRow, guestAttachmentURL = oldGrant, oldCall, oldRow, oldURL
	})
	guestGrant = func(context.Context, string, string) (*guestModel.GuestGrant, error) {
		return nil, guestBusiness.ErrInvalidGrant
	}
	var callAsks int
	guestCallBoard = func(_ context.Context, tok string) (string, error) {
		callAsks++
		switch tok {
		case "in-call":
			return board, nil
		case "left-call":
			return "", guestBusiness.ErrInvalidGrant
		case "store-down":
			return "", errors.New("postgres: connection refused")
		}
		return "", guestBusiness.ErrNotCallToken
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
		return "https://media.example.test/" + objKey, nil
	}
	get := func(h http.HandlerFunc, tok, obj string) int {
		r := httptest.NewRequest(http.MethodGet, "/guest/x/"+tok+"/"+obj, nil)
		rc := chi.NewRouteContext()
		rc.URLParams.Add("token", tok)
		rc.URLParams.Add("obj_uuid", obj)
		rec := httptest.NewRecorder()
		h(rec, r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc)))
		return rec.Code
	}

	if code := get(GuestBoardAttachment, "in-call", "on-board"); code != http.StatusTemporaryRedirect {
		t.Fatalf("the stage board's picture: %d", code)
	}
	for _, c := range []struct{ tok, obj string }{
		{"in-call", "other-board"},
		{"left-call", "on-board"},
		{"a-link-gone", "on-board"},
	} {
		if code := get(GuestBoardAttachment, c.tok, c.obj); code != http.StatusNotFound {
			t.Errorf("%s/%s: %d; want the uniform not-available", c.tok, c.obj, code)
		}
	}
	if code := get(GuestBoardAttachment, "store-down", "on-board"); code != http.StatusServiceUnavailable {
		t.Errorf("a call read that failed: %d; want 503", code)
	}
	// A call's token opens a board's pictures only, never a doc's.
	callAsks = 0
	if code := get(GuestDocAttachment, "in-call", "in-doc"); code != http.StatusNotFound || callAsks != 0 {
		t.Errorf("a doc's picture by a call's token: %d, the call asked %d times", code, callAsks)
	}
	if len(signed) != 1 {
		t.Errorf("signed %v: only the stage board's picture may be", signed)
	}
}
