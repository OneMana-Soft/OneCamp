package controllers

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// uploadAs posts a file with jsonData as /uploadFile's sign-in leaves the
// request: the person's Postgres account, and no graph identity.
func uploadAs(t *testing.T, person uuid.UUID, jsonData string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "notes.txt")
	_, _ = part.Write([]byte("hello"))
	_ = mw.WriteField("jsonData", jsonData)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/uploadFile", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var u models.UserInfo
	u.UserPostgresInfo.Id = person
	req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, u))
	rec := httptest.NewRecorder()
	UploadFile(rec, req)
	return rec
}

// A file goes only where the uploader may write. A kind with no rule (an
// import's task or comment), no kind, and a place named so that it can't be
// theirs, are refused in the uploader's words before anything is stored (the
// store isn't set up here, so reaching it would fail with another code).
func TestAnUploadToAPlaceWithNoRuleOrNotTheirsIsRefused(t *testing.T) {
	me, ann, ben := uuid.New(), uuid.NewString(), uuid.NewString()
	others := helpers.GetGroupingId(ann, ben)
	for name, jsonData := range map[string]string{
		"an import's task":                 `{"src_key":"task","src_value":"` + uuid.NewString() + `"}`,
		"an import's comment":              `{"src_key":"comment","src_value":"` + uuid.NewString() + `"}`,
		"no kind":                          `{"src_value":"` + uuid.NewString() + `"}`,
		"other people's conversation":      `{"src_key":"chat","src_value":"` + others + `"}`,
		"a conversation id that isn't one": `{"src_key":"chat","src_value":"` + me.String() + `"}`,
		"a group id that isn't one":        `{"src_key":"grpChat","src_value":"not-a-group"}`,
		"several places, one of them other people's conversation": `{"src_key":"chat","chat_uuids":["` + others + `"]}`,
	} {
		rec := uploadAs(t, me, jsonData)
		if rec.Code != http.StatusForbidden || !bytes.Contains(rec.Body.Bytes(), []byte("You can't add files here.")) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}
