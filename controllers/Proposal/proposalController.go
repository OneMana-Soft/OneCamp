// Package controllers (Proposal) answers /proposals, the edit proposals API
// (business/Proposal), and the two internal routes the collaboration service's
// applyProposal op calls back.
package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Proposal"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
)

// A proposal's JSON is capped well under the member router's 8 MiB body limit:
// base, proposed and hunks together are at most 1 MiB in the service.
const maxBody = 3 << 20

func write(w http.ResponseWriter, r *http.Request, where string, err error) {
	var invalid *business.InvalidError
	var conflict *business.ConflictError
	switch {
	case errors.As(err, &invalid):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": invalid.Reason})
	case errors.As(err, &conflict):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
			"msg":       "The text changed since this was suggested. Insert it below, or decline it.",
			"conflicts": conflict.HunkIDs,
		})
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That suggestion isn't there any more."})
	case errors.Is(err, business.ErrForbidden):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You can't do that with this suggestion."})
	case errors.Is(err, business.ErrLocked):
		helpers.WriteJSON(w, http.StatusLocked, helpers.Envolope{"msg": "This doc is locked, so it takes no suggestions."})
	case errors.Is(err, business.ErrBusy):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "Someone is deciding on this suggestion right now. Try again in a moment."})
	case errors.Is(err, business.ErrDecided):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "That suggestion has already been decided."})
	case errors.Is(err, business.ErrTooMany):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "This doc has too many open suggestions. Decide on some first."})
	case errors.Is(err, business.ErrUnavailable):
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Suggestions can't be applied on the server right now. Open the doc to accept it there."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Proposal/%s err: %+v", where, err)
		helpers.WriteRetryLater(w, "Something went wrong.")
	}
}

// subject is the signed-in person, from server context (I11).
func subject(w http.ResponseWriter, r *http.Request) (accessBusiness.Subject, bool) {
	_, who := accessBusiness.RequestSubject(r)
	if who.UserUID == "" {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Sign in again."})
		return accessBusiness.Subject{}, false
	}
	return who, true
}

func read(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(into); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That suggestion couldn't be read."})
		return false
	}
	return true
}

// createInput is what a client may send: never who proposed it (the server
// knows), never a server-side draft.
type createInput struct {
	ResourceKind string              `json:"resource_kind"`
	ResourceUUID string              `json:"resource_uuid"`
	ThreadID     string              `json:"thread_id"`
	Anchor       json.RawMessage     `json:"anchor"`
	BaseHash     string              `json:"base_hash"`
	Base         json.RawMessage     `json:"base"`
	Proposed     json.RawMessage     `json:"proposed"`
	Hunks        []business.Hunk     `json:"hunks"`
	Note         string              `json:"note"`
	AuthorKind   business.AuthorKind `json:"author_kind"`
}

// CreateProposal stores a proposal made in the browser. POST /proposals
func CreateProposal(w http.ResponseWriter, r *http.Request) {
	who, ok := subject(w, r)
	if !ok {
		return
	}
	var in createInput
	if !read(w, r, &in) {
		return
	}
	p := business.Proposal{
		ResourceKind: in.ResourceKind, ResourceUUID: in.ResourceUUID, ThreadID: in.ThreadID,
		BaseHash: in.BaseHash, Base: in.Base, Proposed: in.Proposed, Hunks: in.Hunks, Note: in.Note, AuthorKind: in.AuthorKind,
	}
	if len(in.Anchor) > 0 {
		if err := json.Unmarshal(in.Anchor, &p.Anchor); err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That suggestion's place couldn't be read."})
			return
		}
	}
	out, err := business.Proposals.Create(r.Context(), who, p)
	if err != nil {
		write(w, r, "CreateProposal", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": out})
}

// ListProposals is a resource's proposals. GET /proposals?resource_kind=doc&resource_uuid=
func ListProposals(w http.ResponseWriter, r *http.Request) {
	who, ok := subject(w, r)
	if !ok {
		return
	}
	kind := r.URL.Query().Get("resource_kind")
	if kind == "" {
		kind = accessBusiness.KindDoc
	}
	list, err := business.Proposals.List(r.Context(), who, accessBusiness.Resource{Kind: kind, UUID: r.URL.Query().Get("resource_uuid")})
	if err != nil {
		write(w, r, "ListProposals", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"proposals": list}})
}

// GetProposal is one proposal. GET /proposals/{proposal_id}
func GetProposal(w http.ResponseWriter, r *http.Request) {
	who, ok := subject(w, r)
	if !ok {
		return
	}
	p, err := business.Proposals.Get(r.Context(), who, chi.URLParam(r, "proposal_id"))
	if err != nil {
		write(w, r, "GetProposal", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": p})
}

type decideInput struct {
	Accept      bool     `json:"accept"`
	HunkIDs     []string `json:"hunk_ids"`
	Apply       string   `json:"apply"`
	InsertBelow bool     `json:"insert_below"`
}

// DecideProposal accepts or declines some or all of a proposal's changes.
// POST /proposals/{proposal_id}/decide {accept, hunk_ids, apply: server|client, insert_below}
// A 409 with "conflicts" lists the changes whose text moved on.
func DecideProposal(w http.ResponseWriter, r *http.Request) {
	who, ok := subject(w, r)
	if !ok {
		return
	}
	var in decideInput
	if !read(w, r, &in) {
		return
	}
	p, err := business.Proposals.Decide(r.Context(), who, chi.URLParam(r, "proposal_id"), business.Decision{
		Accept: in.Accept, HunkIDs: in.HunkIDs, Apply: in.Apply, InsertBelow: in.InsertBelow,
	})
	if err != nil {
		write(w, r, "DecideProposal", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": p})
}

// WithdrawProposal takes an open proposal back. POST /proposals/{proposal_id}/withdraw
func WithdrawProposal(w http.ResponseWriter, r *http.Request) {
	who, ok := subject(w, r)
	if !ok {
		return
	}
	p, err := business.Proposals.Withdraw(r.Context(), who, chi.URLParam(r, "proposal_id"))
	if err != nil {
		write(w, r, "WithdrawProposal", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": p})
}

// ReopenProposal takes back the caller's own just-made decisions on some changes:
// a reviewer whose editor could not apply what they accepted.
// POST /proposals/{proposal_id}/reopen {hunk_ids}
func ReopenProposal(w http.ResponseWriter, r *http.Request) {
	who, ok := subject(w, r)
	if !ok {
		return
	}
	var in struct {
		HunkIDs []string `json:"hunk_ids"`
	}
	if !read(w, r, &in) {
		return
	}
	p, err := business.Proposals.Reopen(r.Context(), who, chi.URLParam(r, "proposal_id"), in.HunkIDs)
	if err != nil {
		write(w, r, "ReopenProposal", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": p})
}

// ProposalStep is a proposal with the step it waits on, for the collaboration
// service's applyProposal op. GET /docColab/proposals/{proposal_id} (internal secret)
func ProposalStep(w http.ResponseWriter, r *http.Request) {
	s, err := business.Collab.Step(r.Context(), chi.URLParam(r, "proposal_id"))
	if err != nil {
		write(w, r, "ProposalStep", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": s})
}

// ProposalPrepared stores what the op worked out for a server-made proposal.
// POST /docColab/proposals/{proposal_id}/prepared (internal secret)
func ProposalPrepared(w http.ResponseWriter, r *http.Request) {
	var in business.Prepared
	if !read(w, r, &in) {
		return
	}
	if err := business.Collab.Prepared(r.Context(), chi.URLParam(r, "proposal_id"), in); err != nil {
		write(w, r, "ProposalPrepared", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok"})
}
