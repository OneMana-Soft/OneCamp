package docController

import (
	"encoding/json"
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/helpers"
)

// setColors colours docs (a seam for tests).
var setColors = business.SetColors

// colorRequest is a colouring as POST /doc/color takes it: the docs, and a
// camp hue's name, or null to clear.
type colorRequest struct {
	DocUUIDs []string `json:"doc_uuids"`
	Color    *string  `json:"color"`
}

// SetDocColor is POST /doc/color {doc_uuids, color} (docs F1, the F1a/F1b
// contract 6): up to 200 docs coloured in one write, a camp hue or none
// (null), each one the person may edit, or none at all; one they may not
// open is not found, as a doc they can't open always is. It answers
// {data: {count}}: how many changed.
func SetDocColor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, signedIn := httpx.MustUser(w, r)
	if !signedIn {
		return
	}
	who := accessBusiness.SubjectOf(&user)
	var in colorRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "That isn't a colouring.")
		return
	}
	color := ""
	if in.Color != nil {
		color = *in.Color
		if !helpers.IsCampHue(color) {
			badRequest(w, "That isn't one of the colours.")
			return
		}
	}
	seen := map[string]bool{}
	var ids []string
	for _, id := range in.DocUUIDs {
		if !helpers.IsCanonicalUUID(id) {
			badRequest(w, "That isn't a doc.")
			return
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	switch {
	case len(ids) == 0:
		badRequest(w, "Say which docs to colour.")
		return
	case len(ids) > business.ColorCap:
		badRequest(w, "At most 200 can be coloured at once.")
		return
	}
	res := make([]accessBusiness.Resource, len(ids))
	for i, id := range ids {
		res[i] = docRes(id)
	}
	decided, err := accessBusiness.ResolveAll(ctx, who, res)
	if err != nil {
		treeFailed(w, r, "SetDocColor", err)
		return
	}
	for _, one := range res {
		d, ok := decided[one]
		switch {
		case !ok, d.Level == accessBusiness.LevelNone:
			helpers.WriteDocNotFound(w)
			return
		case d.Level < accessBusiness.LevelEdit:
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"code": "forbidden", "msg": "You can't change some of these."})
			return
		}
	}
	n, err := setColors(ctx, ids, color, business.Actor{UserUUID: who.UserUUID, AgentUUID: who.AgentUUID})
	if err != nil {
		treeFailed(w, r, "SetDocColor", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"count": n}})
}
