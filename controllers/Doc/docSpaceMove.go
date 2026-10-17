package docController

// A move that changes a page's space, over HTTP. Everyone in a project's or a
// team's space can open every page in it, so the move shares the page with a
// space's people, or takes it from them: it is cleared as a change to the
// page's sharing before it is made, and followed as one after
// (business/Access space.go).

import (
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	treeBusiness "github.com/akashc777/OneCamp/business/Tree"
)

// clearSpaceMove clears a move that changes a page's space (kind is the
// space's, or treeBusiness.SpaceNone to take the page out of any), answering
// when it may not be made (ok false). nil for a move that keeps the page's
// space. A workspace admin may put a page into a space they aren't in (who,
// from RequestSubject, says whether they are one).
func clearSpaceMove(w http.ResponseWriter, r *http.Request, who accessBusiness.Subject, docUUID, kind, spaceUUID, where string) (*accessBusiness.SpaceMove, bool) {
	if kind == "" {
		return nil, true
	}
	if kind == treeBusiness.SpaceNone {
		kind, spaceUUID = "", ""
	}
	sm, err := accessBusiness.PrepareSpaceMove(r.Context(), who, docRes(docUUID), kind, spaceUUID)
	if err != nil {
		treeFailed(w, r, where, err)
		return nil, false
	}
	return sm, true
}
