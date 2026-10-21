// Package controllers is Who knows' HTTP surface (DG9): who knows about some
// words, and a person's own choice to be left out. Thin: business/WhoKnows
// decides everything, as the signed-in member (through a token, a member at
// most: accessBusiness.SubjectFor). Members only (the Member router): a guest
// link is not a session and never reaches it.
package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/WhoKnows"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// askerOf is who a request asks as, and false for no one signed in.
func askerOf(r *http.Request) (business.Asker, bool) {
	u, ok := userModels.FromContext(r.Context())
	if !ok || u == nil || u.UserDgraphInfo.Uuid == "" {
		return business.Asker{}, false
	}
	a := business.Asker{Subject: accessBusiness.SubjectFor(r.Context(), u), Email: u.UserPostgresInfo.EmailID}
	for _, c := range u.UserDgraphInfo.Channels {
		if c != nil {
			a.Channels = append(a.Channels, c.Uuid)
		}
	}
	for _, p := range u.UserDgraphInfo.Projects {
		if p != nil {
			a.Projects = append(a.Projects, p.Uuid)
		}
	}
	for _, t := range u.UserDgraphInfo.Teams {
		if t != nil {
			a.Teams = append(a.Teams, t.Uuid)
		}
	}
	return a, true
}

// GetWhoKnows GET /people/who-knows?q=&limit=: the people whose work on the
// words the asker can read, ranked, each with up to three items.
func GetWhoKnows(w http.ResponseWriter, r *http.Request) {
	asker, ok := askerOf(r)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	// The house rule for a list's size (helpers.SizeParam): a bad one is the
	// default, one above the most is the most.
	limit := helpers.LimitParam(r.URL.Query(), business.DefaultLimit, business.MaxLimit)
	got, err := business.Ask(r.Context(), asker, r.URL.Query().Get("q"), limit)
	switch {
	case errors.Is(err, business.ErrBadRequest):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Ask about something in 2 to 200 characters."})
	case errors.Is(err, business.ErrNoOne):
		helpers.WriteUnauthorized(w)
	case err != nil:
		helpers.LogErrorWithContext(r.Context(), "controllers/GetWhoKnows err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't work out who knows just now. Try again."})
	default:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Read", "data": got})
	}
}

// GetMyWhoKnows GET /people/who-knows/me: whether the person may be suggested.
func GetMyWhoKnows(w http.ResponseWriter, r *http.Request) {
	asker, ok := askerOf(r)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	listed, err := business.Listed(r.Context(), asker.Subject.UserUUID)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/GetMyWhoKnows err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't read your setting just now. Try again."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]bool{"listed": listed}})
}

// SetMyWhoKnows PUT /people/who-knows/me {listed}: the person's own choice.
// Their own only: there is no id in the request.
func SetMyWhoKnows(w http.ResponseWriter, r *http.Request) {
	asker, ok := askerOf(r)
	if !ok {
		helpers.WriteUnauthorized(w)
		return
	}
	var in struct {
		Listed *bool `json:"listed"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil || in.Listed == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say whether to show you: {\"listed\": true} or false."})
		return
	}
	if err := business.SetListed(r.Context(), asker.Subject.UserUUID, *in.Listed); err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/SetMyWhoKnows err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't save that just now. Try again."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]bool{"listed": *in.Listed}})
}
