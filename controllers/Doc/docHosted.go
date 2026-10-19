package docController

// A hosted doc (a table row's page, docs D-18) refused by a docs path: every
// path but the three that open hosted docs decides one as none, and answers
// here. Someone its host lets view it is told where it is, so the doc page can
// take them there: 403 {code:"hosted", msg:"This page belongs to a row of
// Deals. Change it there.", path}. Someone it was there for before its host
// went to the trash is told it was deleted (410). Anyone else gets exactly
// what a doc that isn't there gets, the same status and the same body, since
// the place names the table: a stranger learns nothing.

import (
	"context"
	"errors"
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
)

// The reads refuseHosted makes: seams, so a test can answer them.
var (
	hostOfDoc   = business.HostOf
	hostedPlace = business.HostPlaceOf
	decideDoc   = func(ctx context.Context, who accessBusiness.Subject, docUUID string) (accessBusiness.Decision, error) {
		return accessBusiness.Rules.Resolve(ctx, who, docRes(docUUID))
	}
)

// refuseHosted answers a refused doc when it is hosted, and says whether it
// answered; for a doc with no host it answers nothing, and the path refuses
// it as it does any doc. The host is decided for who, opened for this answer
// alone (what to tell them), never for the doc's contents.
func refuseHosted(ctx context.Context, w http.ResponseWriter, who accessBusiness.Subject, docUUID string) bool {
	host, err := hostOfDoc(ctx, docUUID)
	switch {
	case errors.Is(err, business.ErrNotFound):
		return false
	case err != nil:
		helpers.LogErrorWithContext(ctx, "controllers/refuseHosted %s: %+v", docUUID, err)
		helpers.WriteRetryLater(w, "Couldn't open the doc just now.")
		return true
	case host.Kind == "":
		return false
	}
	d, err := decideDoc(business.WithHostedOpen(ctx), who, docUUID)
	switch {
	case errors.Is(err, accessBusiness.ErrNotFound):
		helpers.WriteDocNotFound(w)
		return true
	case err != nil:
		helpers.LogErrorWithContext(ctx, "controllers/refuseHosted %s: the host couldn't be decided: %+v", docUUID, err)
		helpers.WriteRetryLater(w, "Couldn't open the doc just now.")
		return true
	case d.Deleted && d.Was >= accessBusiness.LevelView:
		docGone(w, true)
		return true
	case d.Deleted || d.Level < accessBusiness.LevelView:
		helpers.WriteDocNotFound(w)
		return true
	}
	place, err := hostedPlace(ctx, host)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/refuseHosted %s: where its host is couldn't be read: %+v", docUUID, err)
		helpers.WriteRetryLater(w, "Couldn't open the doc just now.")
		return true
	}
	helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": place.Belongs, "code": "hosted", "path": place.Path})
	return true
}
