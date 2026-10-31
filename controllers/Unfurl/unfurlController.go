// Package controllers (Unfurl) answers /unfurl: link previews read by this
// server (business/Unfurl), the sites an admin added for doc embeds, and
// pictures from a link kept with a doc (docs programme B4).
package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	business "github.com/akashc777/OneCamp/business/Unfurl"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/redis/registry"
)

// Seams for tests: the business calls, the admin's embed list and the
// person's fetch window.
var (
	unfurlCard  = business.Unfurl
	storeRemote = business.StoreRemote
	embedHosts  = func(ctx context.Context) []string {
		return policyBusiness.Policies.List(ctx, policyBusiness.EmbedAllowlist)
	}
	// The person's fetch window (business.AllowFetch, FetchesPerMinute).
	allowFetch = business.AllowFetch
)

// errLimited is a person over their fetches for the minute.
type errLimited struct{ retryAfter int }

func (errLimited) Error() string { return "unfurl: too many fetches" }

// budget is the person's fetch limit for one action, counted in Redis, or in
// this process while Redis can't be asked (the limit still holds).
//
// Not customMiddleware.IPRateLimit: it lets GETs through uncounted on purpose,
// and it counts by address, so an office behind one address would share a
// bucket. This counts the person, and only the fetches the server really
// makes (business/Unfurl asks it on a cache miss).
func budget(r *http.Request, action, userUUID string) func() error {
	return func() error {
		allowed, retry := allowFetch(r.Context(), action, userUUID)
		if allowed {
			return nil
		}
		if retry <= 0 {
			retry = int(registry.UnfurlRate.TTL.Seconds())
		}
		return errLimited{retryAfter: retry}
	}
}

func limited(w http.ResponseWriter, err error) bool {
	var l errLimited
	if !errors.As(err, &l) {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(l.retryAfter))
	helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{
		"msg":    "Too many links previewed in a minute. Wait a moment and try again.",
		"status": "rate_limited",
	})
	return true
}

// who is the signed-in person, from server context (I11).
func who(w http.ResponseWriter, r *http.Request) (accessBusiness.Subject, bool) {
	_, s := accessBusiness.RequestSubject(r)
	if s.UserUID == "" || s.UserUUID == "" {
		helpers.WriteUnauthorized(w)
		return accessBusiness.Subject{}, false
	}
	return s, true
}

// Unfurl GET /unfurl?url= answers a link's preview card. A preview the server
// couldn't read is still a 200: the card says it wasn't fetched and why, and
// the doc shows a plain link.
func Unfurl(w http.ResponseWriter, r *http.Request) {
	person, ok := who(w, r)
	if !ok {
		return
	}
	card, err := unfurlCard(r.Context(), r.URL.Query().Get("url"), budget(r, "preview", person.UserUUID))
	if errors.Is(err, business.ErrInvalidURL) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Give a link that starts with http:// or https://."})
		return
	}
	if limited(w, err) {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/Unfurl/Unfurl err: %+v", err)
		helpers.WriteRetryLater(w, "The preview couldn't be made.")
		return
	}
	// A card is the same for everyone: the browser may keep it an hour. A
	// failure is asked again sooner (the server remembers it 10 minutes).
	if card.Fetched {
		w.Header().Set("Cache-Control", "private, max-age=3600")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=600")
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": card})
}

// EmbedSites GET /unfurl/embeds answers the sites an admin added to the ones
// docs can embed (the policy embed.allowlist). The built-in sites live in the
// web app; this is only the admin's additions.
func EmbedSites(w http.ResponseWriter, r *http.Request) {
	if _, ok := who(w, r); !ok {
		return
	}
	hosts := embedHosts(r.Context())
	if hosts == nil {
		hosts = []string{}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string][]string{"hosts": hosts}})
}

type storeRequest struct {
	DocUUID string `json:"doc_uuid"`
	URL     string `json:"url"`
	Kind    string `json:"kind"`
}

// notStoredMessages say why a picture wasn't kept, and what to do.
var notStoredMessages = map[string]string{
	business.ReasonBlocked:     "That picture is on an address inside your network, so it can't be added from a link. Upload the file instead.",
	business.ReasonUnreachable: "Your server couldn't reach that picture. Upload the file instead.",
	business.ReasonTimeout:     "That picture took too long to arrive. Try again, or upload the file.",
	business.ReasonBadStatus:   "The site didn't send that picture. Check the link, or upload the file.",
	business.ReasonTooLarge:    "That picture is over the upload limit for this workspace.",
	business.ReasonNotImage:    "That link isn't to a picture this doc can show (PNG, JPEG, GIF, WebP or AVIF).",
}

// StoreRemote POST /unfurl/store keeps the picture at a link with a doc, for
// someone who can edit it: {doc_uuid, url, kind: "image" | "icon"}.
func StoreRemote(w http.ResponseWriter, r *http.Request) {
	person, ok := who(w, r)
	if !ok {
		return
	}
	var req storeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That request couldn't be read."})
		return
	}
	stored, err := storeRemote(r.Context(), person, req.DocUUID, req.URL, req.Kind, budget(r, "store", person.UserUUID))
	var notStored *business.NotStoredError
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": stored})
	case limited(w, err):
	case errors.Is(err, business.ErrInvalidURL):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Give a link that starts with http:// or https://."})
	case errors.Is(err, business.ErrBadKind):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say whether it's an image or an icon."})
	case errors.Is(err, business.ErrNoSuchDoc):
		helpers.WriteDocNotFound(w)
	case errors.Is(err, business.ErrCantEdit):
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You can't edit this doc, so nothing can be added to it."})
	case errors.As(err, &notStored):
		msg, known := notStoredMessages[notStored.Reason]
		if !known {
			msg = "That picture couldn't be added. Upload the file instead."
		}
		helpers.WriteJSON(w, http.StatusUnprocessableEntity, helpers.Envolope{"msg": msg, "reason": notStored.Reason})
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Unfurl/StoreRemote err: %+v", err)
		helpers.WriteRetryLater(w, "The picture couldn't be added.")
	}
}
