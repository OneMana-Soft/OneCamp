package controllers

import (
	"net/http"

	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
)

// GetEmailLogo handles GET /public/email/logo: the workspace's logo, which its
// emails carry and a form's page wears when the form has none of its own, as
// a 302 to the stored file (httpx.RedirectToStored); 404 when it has none.
func GetEmailLogo(w http.ResponseWriter, r *http.Request) {
	key := configModels.EmailLogo()
	if key == "" {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Logo not found"})
		return
	}
	httpx.RedirectToStored(w, r, key, "logo")
}
