// Package controllers is the work graph's HTTP surface (DG5): the Connected
// panel's read and "how is this connected to…". Thin: business/WorkGraph
// decides everything, as the signed-in member (or, through a token, as at
// most a member: accessBusiness.SubjectFor).
package controllers

import (
	"errors"
	"net/http"
	"strconv"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// subject is who a request reads as.
func subject(r *http.Request) accessBusiness.Subject {
	u, ok := userModels.FromContext(r.Context())
	if !ok {
		return accessBusiness.Subject{}
	}
	return accessBusiness.SubjectFor(r.Context(), u)
}

// answer writes a work-graph error: a bad request, "not found" for a thing
// that isn't there or that the reader can't see (the same words for both),
// and 503 for the graph not answering.
func answer(w http.ResponseWriter, r *http.Request, where string, err error) {
	switch {
	case errors.Is(err, business.ErrBadRequest):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't something the work graph can look up."})
	case errors.Is(err, business.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"code": "not_found", "msg": "That isn't there, or you can't open it."})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/%s err: %+v", where, err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't read what this connects to just now. Try again."})
	}
}

// GetConnected GET /graph/connected?kind=&uuid=&depth=1|2&kinds=doc,task:
// what is connected to a thing, grouped by kind, each with its path.
func GetConnected(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	depth := 1
	if d := q.Get("depth"); d != "" {
		n, err := strconv.Atoi(d)
		if err != nil {
			answer(w, r, "GetConnected", business.ErrBadRequest)
			return
		}
		depth = n
	}
	kinds, err := business.ParseKinds(q.Get("kinds"))
	if err != nil {
		answer(w, r, "GetConnected", err)
		return
	}
	got, err := business.NeighboursOf(r.Context(), subject(r), business.Ref{Kind: q.Get("kind"), ID: q.Get("uuid")}, depth, kinds)
	if err != nil {
		answer(w, r, "GetConnected", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Read", "data": got})
}

// GetPath GET /graph/path?from_kind=&from=&to_kind=&to=: at most three
// shortest ways one thing connects to another, through what the reader may see.
func GetPath(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	got, err := business.PathsBetween(r.Context(), subject(r),
		business.Ref{Kind: q.Get("from_kind"), ID: q.Get("from")},
		business.Ref{Kind: q.Get("to_kind"), ID: q.Get("to")})
	if err != nil {
		answer(w, r, "GetPath", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Read", "data": got})
}
