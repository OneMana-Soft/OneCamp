package controllers

import (
	"context"
	"errors"
	"net/http"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
	business "github.com/akashc777/OneCamp/business/User"
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
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such file."})
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
// (channel_uuids, and chat_uuids, which name conversations by grouping id),
// each once.
func placesSentTo(channelUUIDs, chatGroupingIDs []string) []fileAccessBusiness.Place {
	seen := map[[2]string]bool{}
	var places []fileAccessBusiness.Place
	add := func(kind string, ids []string) {
		for _, id := range ids {
			if !seen[[2]string{kind, id}] {
				seen[[2]string{kind, id}] = true
				places = append(places, fileAccessBusiness.Place{Kind: kind, ID: id})
			}
		}
	}
	add(postgressStruct.ATTACHMENT_SRC_CHANNEL, channelUUIDs)
	add(postgressStruct.ATTACHMENT_SRC_CHAT, chatGroupingIDs)
	return places
}

// serveFileOf answers a request for a file (obj_uuid) of place with its
// address, for someone who may have the place's files
// (fileAccessBusiness.ReadFile): as JSON for the app to fetch (a message's
// files), or as a redirect for a page to load directly (a picture in a doc or
// on a board). The address is signed and short-lived.
func serveFileOf(w http.ResponseWriter, r *http.Request, place fileAccessBusiness.Place, redirect bool) {
	ctx := r.Context()
	user := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)
	att, err := fileAccessBusiness.ReadFile(ctx, user.UserPostgresInfo.Id.String(), place, chi.URLParam(r, "obj_uuid"))
	if !answerFileAccess(ctx, w, err, place, "You can't open this file.", "Couldn't open the file.") {
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
