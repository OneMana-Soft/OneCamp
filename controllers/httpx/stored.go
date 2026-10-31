package httpx

import (
	"net/http"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/minioInit"
)

// RedirectToStored answers a public page's picture (the workspace's email
// logo, a form's cover or logo) with a 302 to a link to the stored file that
// lasts a minute: 302, not 301, since the link changes every time. what names
// the picture in the words of a failure.
func RedirectToStored(w http.ResponseWriter, r *http.Request, key, what string) {
	url, err := minioInit.MinioClient.PresignedGetObject(r.Context(), helpers.UserUploadBucket(), key, time.Minute, nil)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/httpx RedirectToStored %s err: %+v", what, err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't get the " + what + ". Try again."})
		return
	}
	http.Redirect(w, r, url.String(), http.StatusFound)
}
