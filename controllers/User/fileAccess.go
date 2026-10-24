package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
	business "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

// mayAddFileTo answers for an upload: whether the uploader may add a file to
// the place it names, by the place's own rule (fileAccessBusiness.MayAdd).
// ok is false when it answered.
func mayAddFileTo(ctx context.Context, w http.ResponseWriter, user models.UserInfo, place fileAccessBusiness.Place) bool {
	err := fileAccessBusiness.MayAdd(ctx, user.UserPostgresInfo.Id.String(), place)
	return answerFileAccess(ctx, w, err, place, "You can't add files here.", "Couldn't upload the file.")
}

// answerFileAccess answers a file's place, in the person's words: a no is
// 403; a file that isn't there, or isn't the place's, 404; a doc gives its
// own answers, 404 to someone it isn't there for and 410 to someone it was
// there for once deleted; a lookup that failed is 503, to be tried again. ok
// is true when the answer was yes, and nothing was written.
func answerFileAccess(ctx context.Context, w http.ResponseWriter, err error, place fileAccessBusiness.Place, refused, failed string) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, fileAccessBusiness.ErrRefused):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "forbidden", "msg": refused})
	case errors.Is(err, fileAccessBusiness.ErrNoSuchFile):
		helpers.WriteNotFound(w, "There's no such file.")
	case errors.Is(err, docBusiness.ErrNotFound):
		helpers.WriteDocNotFound(w)
	case errors.Is(err, docBusiness.ErrDocDeleted):
		helpers.WriteJSON(w, http.StatusGone, helpers.Envolope{"msg": "This doc was deleted.", "deleted": true})
	default:
		helpers.LogErrorWithContext(ctx, "controllers/answerFileAccess %q %q err: %+v", place.Kind, place.ID, err)
		helpers.WriteRetryLater(w, failed)
	}
	return false
}

// placesSentTo are the places a file sent to several at once goes to
// (channel_uuids, and chat_uuids, which name conversations by grouping id:
// a DM's or a group's, as ConversationOf tells them apart), each once.
func placesSentTo(channelUUIDs, chatGroupingIDs []string) []fileAccessBusiness.Place {
	seen := map[[2]string]bool{}
	var places []fileAccessBusiness.Place
	add := func(p fileAccessBusiness.Place) {
		if key := [2]string{p.Kind, p.ID}; !seen[key] {
			seen[key] = true
			places = append(places, p)
		}
	}
	for _, id := range channelUUIDs {
		add(fileAccessBusiness.Place{Kind: postgressStruct.ATTACHMENT_SRC_CHANNEL, ID: id})
	}
	for _, id := range chatGroupingIDs {
		add(fileAccessBusiness.ConversationOf(id))
	}
	return places
}

// serveFileOf answers a request for a file (obj_uuid) of place with its
// address, for someone who may have the place's files
// (fileAccessBusiness.ReadFile): as JSON for the app to fetch (a message's
// files), or as a redirect for a page to load directly (a picture in a doc or
// on a board). The address is signed and short-lived.
//
// Any of these routes redirects when asked (?redirect=1), so the web app can
// put the route itself in an <img src> and the browser may reuse the redirect
// for a few minutes (mediaRedirect), rather than the app re-signing and
// re-downloading. Access to the place is checked on every ask, and the
// signature stays short (business.PlaceFileURLTTL), so a member who loses the
// place loses its files within minutes.
func serveFileOf(w http.ResponseWriter, r *http.Request, place fileAccessBusiness.Place, redirect bool) {
	ctx := r.Context()
	user, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}
	att, err := fileAccessBusiness.ReadFile(ctx, user.UserPostgresInfo.Id.String(), place, chi.URLParam(r, "obj_uuid"))
	if !answerFileAccess(ctx, w, err, place, "You can't open this file.", "Couldn't open the file.") {
		return
	}
	if r.URL.Query().Get("redirect") == "1" {
		mediaRedirect(w, r, att.ObjKey, business.PlaceFileURLTTL, business.PlaceFileRedirectMaxAge)
		return
	}
	url, err := business.GetFileURLByObjectName(ctx, att.ObjKey)
	switch {
	case err != nil:
		helpers.LogErrorWithContext(ctx, "controllers/serveFileOf signing file %s err: %+v", att.Uuid, err)
		helpers.WriteRetryLater(w, "Couldn't open the file.")
	case redirect:
		http.Redirect(w, r, url, http.StatusTemporaryRedirect)
	default:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got the url!", "url": url})
	}
}

// mediaRedirect answers with a redirect to the stored object at objKey,
// signed for ttl, that the browser may keep for maxAge (inside ttl): private,
// so no shared cache holds it.
func mediaRedirect(w http.ResponseWriter, r *http.Request, objKey string, ttl, maxAge time.Duration) {
	url, err := business.GetFileURLByObjectNameFor(r.Context(), objKey, ttl)
	if err != nil || url == "" {
		helpers.LogErrorWithContext(r.Context(), "controllers/mediaRedirect signing %s err: %+v", objKey, err)
		helpers.WriteRetryLater(w, "Couldn't open the picture.")
		return
	}
	w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(maxAge.Seconds())))
	http.Redirect(w, r, url, http.StatusFound)
}

// ServeAvatar answers GET /avatar/{obj_uuid} for someone signed in with a
// redirect to a profile picture (a public attachment), which the web app
// puts straight in an <img src>: a picture is fetched once and cached, where
// each avatar on screen was a getFile request (a lookup and a signature)
// repeated every four minutes, 30 to 80 on a cold load. Anything that isn't a
// profile picture is not found.
func ServeAvatar(w http.ResponseWriter, r *http.Request) {
	if _, signedIn := httpx.MustUser(w, r); !signedIn {
		return
	}
	id, validID := helpers.UUIDParam(w, r, "obj_uuid")
	if !validID {
		return
	}
	att, err := attachmentBusiness.GetAttachmentByObjUUID(r.Context(), id.String(), postgressStruct.ATTACHMENT_SRC_PUBLIC)
	if err != nil || att == nil || att.SrcKey != postgressStruct.ATTACHMENT_SRC_PUBLIC || att.ObjKey == "" {
		helpers.WriteNotFound(w, "That picture isn't there.")
		return
	}
	mediaRedirect(w, r, att.ObjKey, business.AvatarURLTTL, business.AvatarRedirectMaxAge)
}
