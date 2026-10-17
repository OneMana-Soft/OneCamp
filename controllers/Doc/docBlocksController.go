package docController

// Blocks moved to another doc (docs programme B1, "Move to…" in the block
// menu, with B6's dialog):
//
//	POST /doc/blocks/append {doc_uuid, content}   content: the editor's JSON of the blocks
//
// The blocks go at the end of the doc, live (whoever has it open sees them
// arrive). It takes edit access to that doc, decided by the Access Resolver;
// the browser then deletes the blocks from the doc they came from.

import (
	"encoding/json"
	"errors"
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	business "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/services/collab"
)

type appendBlocksRequest struct {
	DocUUID string             `json:"doc_uuid"`
	Content *contentcodec.Node `json:"content"`
}

// AppendDocBlocks adds blocks at the end of a doc the person can edit.
func AppendDocBlocks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in appendBlocksRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "Failed to parse the body of the req")
		return
	}
	if !helpers.IsCanonicalUUID(in.DocUUID) {
		badRequest(w, "Invalid doc id")
		return
	}
	if in.Content == nil {
		badRequest(w, "There are no blocks to move.")
		return
	}
	_, who := accessBusiness.RequestSubject(r)
	d, err := accessBusiness.Rules.Resolve(ctx, who, docRes(in.DocUUID))
	switch {
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteDocNotFound(w)
		return
	case err != nil:
		helpers.LogErrorWithContext(ctx, "controllers/AppendDocBlocks access err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't open that doc just now. Try again in a moment."})
		return
	case d.Deleted:
		// To someone it was there for, the trash; to anyone else, nothing.
		docGone(w, d.Level >= accessBusiness.LevelEdit || d.Owner)
		return
	case d.Level < accessBusiness.LevelEdit:
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You can't edit that doc."})
		return
	}
	res, err := business.AppendBlocksLive(ctx, in.DocUUID, in.Content, collab.By{UserUUID: who.UserUUID})
	if err != nil {
		switch {
		case errors.Is(err, business.ErrNoBlocks):
			badRequest(w, "There are no blocks to move.")
		case errors.Is(err, collab.ErrDocInTrash):
			docGone(w, true)
		default:
			helpers.LogErrorWithContext(ctx, "controllers/AppendDocBlocks live write err: %+v", err)
			status, msg := liveWriteStatus(err)
			helpers.WriteJSON(w, status, helpers.Envolope{"msg": msg})
		}
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Added", "data": helpers.Envolope{"blocks": res.Blocks, "persisted": res.Persisted}})
}
