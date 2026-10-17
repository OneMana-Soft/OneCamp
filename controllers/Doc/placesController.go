package docController

// Places over HTTP (router/docsPlaces.go): a person's stars and pins, of any
// kind of thing, in their order. Docs are a kind of thing here: their
// resolver, below, names only the docs the Access Resolver says the person
// can open, so a starred doc they lost access to drops out of their list.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Doc"
	placesBusiness "github.com/akashc777/OneCamp/business/Places"
	"github.com/akashc777/OneCamp/helpers"
)

// docPlaces resolves docs for places.
type docPlaces struct{}

func (docPlaces) Resolve(ctx context.Context, who placesBusiness.Person, ids []string) (map[string]placesBusiness.Seen, error) {
	subject := accessBusiness.Subject{UserUID: who.UID, UserUUID: who.UUID}
	open, err := readableIDs(ctx, subject, ids)
	if err != nil {
		return nil, err
	}
	var keep []string
	for _, id := range ids {
		if open[id] {
			keep = append(keep, id)
		}
	}
	rows, err := business.DocRows(ctx, keep, who.UID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]placesBusiness.Seen, len(rows))
	for _, d := range rows {
		if open[d.Uuid] {
			out[d.Uuid] = placesBusiness.Seen{Title: d.Title, Icon: d.Icon, Href: "/app/doc/" + d.Uuid}
		}
	}
	return out, nil
}

func init() {
	placesBusiness.Register(accessBusiness.KindDoc, docPlaces{})
}

func placeFailed(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, placesBusiness.ErrKind), errors.Is(err, placesBusiness.ErrResource), errors.Is(err, placesBusiness.ErrNotPlaced):
		badRequest(w, err.Error())
	case errors.Is(err, placesBusiness.ErrCantOpen):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such thing to star."})
	default:
		helpers.LogErrorWithContext(ctx, "controllers/places err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Something went wrong. Try again."})
	}
}

func placesPerson(r *http.Request) placesBusiness.Person {
	_, who := accessBusiness.RequestSubject(r)
	return placesBusiness.Person{UUID: who.UserUUID, UID: who.UserUID}
}

// GetPlaces is the person's places of one kind (GET /places?kind=star
// [&resource_kind=doc]), in their order, each named.
func GetPlaces(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = placesBusiness.Star
	}
	items, err := placesBusiness.List(ctx, placesPerson(r), kind, r.URL.Query().Get("resource_kind"))
	if err != nil {
		placeFailed(ctx, w, err)
		return
	}
	if items == nil {
		items = []placesBusiness.Item{}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"places": items}})
}

// AddPlace stars or pins a thing the person can open, after another of
// theirs or last, or moves it there (POST /places).
func AddPlace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in struct {
		Kind         string `json:"kind"`
		ResourceKind string `json:"resource_kind"`
		ResourceUUID string `json:"resource_uuid"`
		AfterUUID    string `json:"after_uuid,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ResourceUUID == "" || len(in.ResourceUUID) > 128 {
		badRequest(w, "Say what to star.")
		return
	}
	item, err := placesBusiness.Add(ctx, placesPerson(r), in.Kind, in.ResourceKind, in.ResourceUUID, in.AfterUUID)
	if err != nil {
		placeFailed(ctx, w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": item})
}

// RemovePlace takes a star or pin away (DELETE /places?kind=&resource_uuid=).
func RemovePlace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	if q.Get("resource_uuid") == "" {
		badRequest(w, "Say what to unstar.")
		return
	}
	if err := placesBusiness.Remove(ctx, placesPerson(r), q.Get("kind"), q.Get("resource_uuid")); err != nil {
		placeFailed(ctx, w, err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Removed."})
}
