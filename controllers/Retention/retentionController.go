package controllers

// Retention rules and holds over HTTP (the governance trail's H2,
// business/Retention): the admin panel's routes, under /admin, so only
// workspace admins reach them (router/calendarRetention.go). Every change,
// search and export is a row in the audit log before it's answered, or it
// isn't answered; holds are silent, so nothing here tells the people on one.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	retentionBusiness "github.com/akashc777/OneCamp/business/Retention"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// maxBody bounds a request's JSON: a hold names at most 1,000 people.
const maxBody = 128 << 10

// kindOut is a kind as the panel lists it.
type kindOut struct {
	Name    string   `json:"name"`
	Label   string   `json:"label"`
	Formats []string `json:"formats"`
}

// fail answers a business error as its status, and logs the unexpected ones.
func fail(w http.ResponseWriter, r *http.Request, err error, what string) {
	switch {
	case errors.Is(err, retentionBusiness.ErrUnknownKind):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such kind of item."})
	case errors.Is(err, retentionBusiness.ErrNotFound):
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such hold."})
	case errors.Is(err, retentionBusiness.ErrReleased):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "That hold was released, so it can't change and its items aren't searched or exported."})
	case errors.Is(err, retentionBusiness.ErrCountMoved):
		// The count now, for the panel to show before the admin saves again.
		var moved *retentionBusiness.CountMoved
		data := map[string]interface{}{}
		if errors.As(err, &moved) {
			data["would_remove"] = moved.Now
		}
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"msg": "The count changed since it was shown. Check the new count, then save again.", "data": data})
	case errors.Is(err, retentionBusiness.ErrBadRequest):
		why := strings.TrimPrefix(err.Error(), retentionBusiness.ErrBadRequest.Error()+": ")
		if why != "" {
			why = strings.ToUpper(why[:1]) + why[1:] + "."
		}
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": why})
	case errors.Is(err, auditBusiness.ErrNotRecorded), errors.Is(err, policyBusiness.ErrNotRecorded):
		helpers.WriteRetryLater(w, "That couldn't be recorded in the audit log, so it wasn't done.")
	default:
		helpers.LogErrorWithContext(r.Context(), "controllers/Retention %s: %+v", what, err)
		helpers.WriteRetryLater(w, "That couldn't be done just now.")
	}
}

// admin is the signed-in admin, or a 401 answered.
func admin(w http.ResponseWriter, r *http.Request) (*userModels.UserInfo, bool) {
	u, ok := userModels.FromContext(r.Context())
	if !ok || u == nil {
		helpers.WriteUnauthorized(w)
		return nil, false
	}
	return u, true
}

// ListRules handles GET /admin/retention/rules: every kind's rule, and the
// kinds with the formats their items export as.
func ListRules(w http.ResponseWriter, r *http.Request) {
	rules, err := retentionBusiness.Rules(r.Context())
	if err != nil {
		fail(w, r, err, "ListRules")
		return
	}
	kinds := []kindOut{}
	for _, k := range retentionBusiness.Kinds() {
		kinds = append(kinds, kindOut{Name: k.Name, Label: k.Label, Formats: k.Formats})
	}
	if rules == nil {
		rules = []retentionBusiness.Rule{}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{"rules": rules, "kinds": kinds}})
}

// PreviewRule handles GET /admin/retention/rules/{kind}/preview?days=N: how
// many items the rule would remove now, which the panel shows before Save.
func PreviewRule(w http.ResponseWriter, r *http.Request) {
	days, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Days are a whole number."})
		return
	}
	n, err := retentionBusiness.Preview(r.Context(), chi.URLParam(r, "kind"), days)
	if err != nil {
		fail(w, r, err, "PreviewRule")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{"days": days, "would_remove": n}})
}

// SetRule handles PUT /admin/retention/rules/{kind} {"days", "shown"}: shown
// is the count the panel showed, which a rule needs before it's saved.
func SetRule(w http.ResponseWriter, r *http.Request) {
	u, ok := admin(w, r)
	if !ok {
		return
	}
	var in struct {
		Days  *int   `json:"days"`
		Shown *int64 `json:"shown"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil || in.Days == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Send the rule's days."})
		return
	}
	by := policyBusiness.Changer{UserID: u.UserPostgresInfo.Id, Email: u.UserPostgresInfo.EmailID}
	rule, err := retentionBusiness.SetRule(r.Context(), by, chi.URLParam(r, "kind"), *in.Days, in.Shown)
	if err != nil {
		fail(w, r, err, "SetRule")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": rule})
}

// ListHolds handles GET /admin/retention/holds: every hold, live ones first.
func ListHolds(w http.ResponseWriter, r *http.Request) {
	holds, err := retentionBusiness.Holds(r.Context())
	if err != nil {
		fail(w, r, err, "ListHolds")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": holds})
}

// holdInput reads a hold's name, note and people.
func holdInput(w http.ResponseWriter, r *http.Request) (retentionBusiness.HoldInput, bool) {
	var in retentionBusiness.HoldInput
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That hold couldn't be read."})
		return in, false
	}
	return in, true
}

// holdID is the {id} in the path, or a 404 answered.
func holdID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such hold."})
		return uuid.Nil, false
	}
	return id, true
}

// PlaceHold handles POST /admin/retention/holds {"name", "note", "add"}.
func PlaceHold(w http.ResponseWriter, r *http.Request) {
	u, ok := admin(w, r)
	if !ok {
		return
	}
	in, ok := holdInput(w, r)
	if !ok {
		return
	}
	h, err := retentionBusiness.PlaceHold(r, u.UserPostgresInfo.Id, in)
	if err != nil {
		fail(w, r, err, "PlaceHold")
		return
	}
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"msg": "ok", "data": h})
}

// ChangeHold handles PATCH /admin/retention/holds/{id} {"name"?, "note"?,
// "add", "remove"}.
func ChangeHold(w http.ResponseWriter, r *http.Request) {
	u, ok := admin(w, r)
	if !ok {
		return
	}
	id, ok := holdID(w, r)
	if !ok {
		return
	}
	in, ok := holdInput(w, r)
	if !ok {
		return
	}
	h, err := retentionBusiness.ChangeHold(r, u.UserPostgresInfo.Id, id, in)
	if err != nil {
		fail(w, r, err, "ChangeHold")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": h})
}

// ReleaseHold handles POST /admin/retention/holds/{id}/release.
func ReleaseHold(w http.ResponseWriter, r *http.Request) {
	u, ok := admin(w, r)
	if !ok {
		return
	}
	id, ok := holdID(w, r)
	if !ok {
		return
	}
	h, err := retentionBusiness.ReleaseHold(r, u.UserPostgresInfo.Id, id)
	if err != nil {
		fail(w, r, err, "ReleaseHold")
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": h})
}

// when reads a time in the query: RFC 3339 or a date ("2026-11-02", from its
// start); empty is open.
func when(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", v)
}

// query reads ?q=&from=&to=, or a 400 answered.
func query(w http.ResponseWriter, r *http.Request) (retentionBusiness.Query, bool) {
	v := r.URL.Query()
	from, err1 := when(v.Get("from"))
	to, err2 := when(v.Get("to"))
	if err1 != nil || err2 != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "From and to are dates (2026-11-02) or times (RFC 3339)."})
		return retentionBusiness.Query{}, false
	}
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "To comes after from."})
		return retentionBusiness.Query{}, false
	}
	q := strings.TrimSpace(v.Get("q"))
	if len(q) > 200 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A search is at most 200 characters."})
		return retentionBusiness.Query{}, false
	}
	return retentionBusiness.Query{Text: q, From: from, To: to}, true
}

// SearchHold handles GET /admin/retention/holds/{id}/items/{kind}?q=&from=&to=:
// the hold's items of kind, deleted ones included, recorded before answered.
func SearchHold(w http.ResponseWriter, r *http.Request) {
	id, ok := holdID(w, r)
	if !ok {
		return
	}
	q, ok := query(w, r)
	if !ok {
		return
	}
	items, err := retentionBusiness.Search(r, id, chi.URLParam(r, "kind"), q)
	if err != nil {
		fail(w, r, err, "SearchHold")
		return
	}
	if items == nil {
		items = []retentionBusiness.Item{}
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": items})
}

// fileSafe keeps a hold's name to what a file name takes everywhere.
var fileSafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// ExportHold handles GET /admin/retention/holds/{id}/export/{kind}?format=&q=&from=&to=&part=:
// one part of the hold's items of kind as one file, recorded before it goes
// out. X-Exported-Items is how many the file holds and X-Export-Part its part;
// X-Export-Next, when more remain, is the part= token for the next part, and a
// file that isn't the whole export is named for its part and says so inside.
func ExportHold(w http.ResponseWriter, r *http.Request) {
	id, ok := holdID(w, r)
	if !ok {
		return
	}
	q, ok := query(w, r)
	if !ok {
		return
	}
	kind, format := chi.URLParam(r, "kind"), r.URL.Query().Get("format")
	x, err := retentionBusiness.Export(r, id, kind, format, q, r.URL.Query().Get("part"))
	if err != nil {
		fail(w, r, err, "ExportHold")
		return
	}
	contentType := "application/json; charset=utf-8"
	if x.Format == "ics" {
		contentType = "text/calendar; charset=utf-8"
	}
	name := strings.Trim(fileSafe.ReplaceAllString(kind+"-hold-"+id.String()[:8], "-"), "-")
	if x.Note != "" {
		name += "-part-" + strconv.Itoa(x.Part)
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.%s"`, name, x.Format))
	w.Header().Set("X-Exported-Items", strconv.Itoa(x.N))
	w.Header().Set("X-Export-Part", strconv.Itoa(x.Part))
	if x.Next != "" {
		w.Header().Set("X-Export-Next", x.Next)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(x.Body)
}
