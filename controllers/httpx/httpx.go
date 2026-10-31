// Package httpx is what a handler reads off its request before it does
// anything: who is signed in, whether they are an admin, and what was sent (a
// JSON body, an upload). Each answers the
// request itself when it can't go on, so a handler reads
//
//	user, ok := httpx.MustUser(w, r)
//	if !ok {
//		return
//	}
//
// rather than asserting the context value (a panic when it is missing, 379
// handlers did) or keeping its own copy (mustUser, requireAdmin, actorFrom).
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/akashc777/OneCamp/helpers"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
)

// UserFrom is the signed-in person the request carries, if any.
func UserFrom(r *http.Request) (*userModel.UserInfo, bool) {
	return userModel.FromContext(r.Context())
}

// MustUser is the signed-in person, answering 401 (and false) when there is
// none. It returns the value the handlers held before, so &user still works.
func MustUser(w http.ResponseWriter, r *http.Request) (userModel.UserInfo, bool) {
	u, ok := UserFrom(r)
	if !ok || u == nil {
		helpers.WriteUnauthorized(w)
		return userModel.UserInfo{}, false
	}
	return *u, true
}

// RequireAdmin is MustUser for a workspace admin: 403 for anyone else.
func RequireAdmin(w http.ResponseWriter, r *http.Request) (userModel.UserInfo, bool) {
	u, ok := MustUser(w, r)
	if !ok {
		return u, false
	}
	if !u.UserPostgresInfo.IsAdmin {
		helpers.LogWarnWithContext(r.Context(), "%s %s refused: %s isn't an admin", r.Method, helpers.RouteOf(r), u.UserPostgresInfo.Id)
		helpers.WriteForbidden(w, "Only workspace admins can do that.")
		return userModel.UserInfo{}, false
	}
	return u, true
}

// ReadUpload reads a multipart upload's file (field "file") from r, under
// the workspace's upload limit of limitMB megabytes: the body is cut off past
// it before anything is parsed (MaxBytesReader), so no upload can fill the
// disk or memory, and one that is too large is answered 413, naming the limit
// as the web's toast reads it ("upload_limit_mb"). Up to 8 MiB of it is held
// in memory and the rest spills to disk, which the server removes when the
// request ends. The form's other fields are r.FormValue's after it. ok is
// false when it answered; the caller closes the file.
func ReadUpload(w http.ResponseWriter, r *http.Request, limitMB int64) (multipart.File, *multipart.FileHeader, bool) {
	const inMemory = 8 << 20
	r.Body = http.MaxBytesReader(w, r.Body, limitMB<<20)
	if err := r.ParseMultipartForm(inMemory); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			helpers.WriteJSON(w, http.StatusRequestEntityTooLarge, helpers.Envolope{
				"msg":             fmt.Sprintf("File exceeds the %d MB upload limit", limitMB),
				"error":           "file_too_large",
				"upload_limit_mb": limitMB,
			})
			return nil, nil, false
		}
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a file upload."})
		return nil, nil, false
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Choose a file to upload."})
		return nil, nil, false
	}
	return file, header, true
}

// ReadJSON decodes r's JSON body into v, reading at most limit bytes of it:
// a longer body is cut off there, so it can't fill memory, and reads as one
// that isn't JSON. ok is false when it answered 400 because the body wasn't.
func ReadJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, limit)).Decode(v); err != nil {
		helpers.WriteBadRequest(w, "Couldn't read that request.")
		return false
	}
	return true
}
