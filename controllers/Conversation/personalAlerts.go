package controllers

// A person's notification keywords and VIPs (chat P3 stage 3, TN-53/54/58):
// read and replace, each for the person asking only.

import (
	"net/http"

	personalAlertBusiness "github.com/akashc777/OneCamp/business/PersonalAlert"
	"github.com/akashc777/OneCamp/helpers"
)

// GetMyKeywords answers the asker's keywords. GET /me/keywords.
func GetMyKeywords(w http.ResponseWriter, r *http.Request) {
	kws, err := personalAlertBusiness.Keywords(r.Context(), who(r).UserPostgresInfo.Id)
	answerAlerts(w, r, kws, err, "controllers/GetMyKeywords")
}

// PutMyKeywords replaces the asker's keywords, {"keywords": [...]}, and
// answers them as kept. PUT /me/keywords.
func PutMyKeywords(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Keywords []string `json:"keywords"`
	}
	if !decode(w, r, &in) {
		return
	}
	kws, err := personalAlertBusiness.SetKeywords(r.Context(), who(r).UserPostgresInfo.Id, in.Keywords)
	answerAlerts(w, r, kws, err, "controllers/PutMyKeywords")
}

// GetMyVIPs answers the asker's VIPs still in the workspace, {uuid, name}.
// GET /me/vips.
func GetMyVIPs(w http.ResponseWriter, r *http.Request) {
	vips, err := personalAlertBusiness.VIPs(r.Context(), who(r).UserPostgresInfo.Id)
	answerAlerts(w, r, vips, err, "controllers/GetMyVIPs")
}

// PutMyVIPs replaces the asker's VIPs, {"vips": [user uuid...]}, keeping the
// workspace's live people other than themself, and answers them as kept.
// PUT /me/vips.
func PutMyVIPs(w http.ResponseWriter, r *http.Request) {
	var in struct {
		VIPs []string `json:"vips"`
	}
	if !decode(w, r, &in) {
		return
	}
	vips, err := personalAlertBusiness.SetVIPs(r.Context(), who(r).UserPostgresInfo.Id, in.VIPs)
	answerAlerts(w, r, vips, err, "controllers/PutMyVIPs")
}

func answerAlerts[T any](w http.ResponseWriter, r *http.Request, list []T, err error, where string) {
	if rf, ok := personalAlertBusiness.IsRefusal(err); ok {
		helpers.WriteJSON(w, rf.Status(), helpers.Envolope{"msg": rf.Msg})
		return
	}
	if err != nil {
		answerFailure(w, r, err, where)
		return
	}
	if list == nil {
		list = []T{}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": list})
}
