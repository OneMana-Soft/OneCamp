package docController

import (
	"encoding/json"
	"errors"
	"net/http"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// The doc session's authorisation (docs programme A2, the Doc Session).
//
// WHY. /docColab/authorize answers 200 only to someone who may edit, so the
// collaboration service refused everyone else and a member who may only read
// or comment got the saved body, up to 30 s old, that never moved. The session
// connects everyone who may read, and the server decides how far: this answers
// every reader with their level, and canEdit says whether their connection may
// write. The collaboration service opens a reader's connection read-only, so
// Hocuspocus drops anything they send.
//
// It answers:
//   - 200 {level, canEdit, locked} to anyone with view access or more;
//   - 403 to someone with none, 404 for no such doc;
//   - 410 for a doc in the trash, to someone who could see it (nobody joins a
//     deleted doc live, its owner included), and 404 to anyone else.
//
// The level comes from the Access Resolver (accessBusiness.Rules), so A5's
// teams, spaces, expiry and locks reach live sockets with no change here.

// docSession is the answer the collaboration service reads.
type docSession struct {
	Msg     string `json:"msg"`
	Level   string `json:"level"`
	CanEdit bool   `json:"canEdit"`
	Locked  bool   `json:"locked"`
}

// sessionFor is a decision as the session states it. A hard lock leaves only
// full access able to write (D-12).
func sessionFor(d accessBusiness.Decision) docSession {
	canEdit := d.Level >= accessBusiness.LevelEdit
	if d.Locked && d.Level < accessBusiness.LevelFull {
		canEdit = false
	}
	return docSession{Msg: "Authorised", Level: d.Level.String(), CanEdit: canEdit, Locked: d.Locked}
}

// DocCollabSession POST /docColab/session {doc_uuid}: the collaboration
// service asks, with the person's bearer token and the internal secret, how
// far they may go with a doc they are opening live.
func DocCollabSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var input adapter.InputUpdateDoc
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.DocId == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req"})
		return
	}

	who := accessBusiness.SubjectOf(&userInfo)
	// The live session opens hosted docs (a table row's page) by their hosts
	// (D-18): one of the three paths that do.
	d, err := accessBusiness.Rules.Resolve(business.WithHostedOpen(ctx), who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: input.DocId})
	if docMissing(w, err) {
		return
	}
	if err != nil {
		if errors.Is(err, accessBusiness.ErrUnknownKind) {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Not a doc"})
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/DocCollabSession Failed to resolve access err: %+v", err)
		// 503, not 4xx: the service tries again, and the person isn't told
		// they have no access when the graph just didn't answer.
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't check access just now"})
		return
	}
	if d.Deleted {
		// 410 to whoever it was there for (the resolver's level before the
		// deletion, Was), as the doc page answers; 404 to anyone else.
		docGone(w, d.Owner || d.Was >= accessBusiness.LevelView || d.Level >= accessBusiness.LevelView)
		return
	}
	if d.Level < accessBusiness.LevelView {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Unauthorized doc access"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, sessionFor(d))
}
