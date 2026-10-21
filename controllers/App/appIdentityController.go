// Package controllers (App) is an installed app's identity in Admin › Apps
// (platform P-API, design/platform/API_CONTRACT.md §3.1): what it may carry and
// reach, and its credentials. Under the admin group, which a session reaches
// and an API token never does; every change is in the audit log.
package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	appBusiness "github.com/akashc777/OneCamp/business/App"
	"github.com/akashc777/OneCamp/helpers"
	tokenModel "github.com/akashc777/OneCamp/models/postgres/ApiToken"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// tokenView is a credential as Admin › Apps shows it: never its secret.
type tokenView struct {
	ID          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	TokenPrefix string     `json:"token_prefix"`
	Scopes      []string   `json:"scopes"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

func viewOf(t *tokenModel.ApiToken) tokenView {
	v := tokenView{ID: t.Id, Name: t.Name, TokenPrefix: t.TokenPrefix, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, CreatedAt: t.CreatedAt, Scopes: []string{}}
	_ = json.Unmarshal([]byte(t.Scopes), &v.Scopes)
	return v
}

func appID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "appId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't an app's id."})
		return uuid.Nil, false
	}
	return id, true
}

// refuse answers an error in words: a gone app is a 404, a refusal the
// business layer explained is a 400 with its reason.
func refuse(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, appBusiness.ErrNotFound) {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such app; it may have been removed."})
		return
	}
	helpers.LogErrorWithContext(r.Context(), "controllers/App: %+v", err)
	helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
}

// GetIdentity is GET /admin/apps/{appId}/identity: what the app may carry and
// reach, and its live credentials.
func GetIdentity(w http.ResponseWriter, r *http.Request) {
	id, ok := appID(w, r)
	if !ok {
		return
	}
	app, err := appBusiness.Get(r.Context(), id)
	if err != nil {
		refuse(w, r, err)
		return
	}
	tokens, err := appBusiness.Tokens(r.Context(), id)
	if err != nil {
		refuse(w, r, err)
		return
	}
	views := make([]tokenView, 0, len(tokens))
	for _, t := range tokens {
		views = append(views, viewOf(t))
	}
	// Its places, named for this admin and with whether the app's own user
	// may open each (business/App DescribePlaces).
	_, viewer := accessBusiness.RequestSubject(r)
	audience, err := appBusiness.DescribePlaces(r.Context(), viewer, app)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/App: describing places: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't read where the app may reach just now. Try again in a moment."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"identity": identityView{Identity: app, Audience: audience}, "tokens": views}})
}

// identityView is an identity with its places described for the admin
// reading: its audience field stands in for the identity's own.
type identityView struct {
	*appBusiness.Identity
	Audience appBusiness.AudienceView `json:"audience"`
}

// SetAccess is PUT /admin/apps/{appId}/access: {scopes, audience}.
func SetAccess(w http.ResponseWriter, r *http.Request) {
	id, ok := appID(w, r)
	if !ok {
		return
	}
	var body struct {
		Scopes   []string             `json:"scopes"`
		Audience appBusiness.Audience `json:"audience"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Send the app's scopes and audience as JSON."})
		return
	}
	app, err := appBusiness.SetAccess(r.Context(), id, body.Scopes, body.Audience)
	if err != nil {
		refuse(w, r, err)
		return
	}
	auditBusiness.Record(r, "app.access_changed", auditBusiness.CategoryApp, "Changed what "+app.Name+" may do",
		map[string]interface{}{"app_id": id.String(), "scopes": app.Scopes, "places": len(app.Audience.Places), "public": app.Audience.Public})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": app})
}

// MintToken is POST /admin/apps/{appId}/tokens: {name, scopes, expires_in_days}.
// The secret is in this answer only.
func MintToken(w http.ResponseWriter, r *http.Request) {
	id, ok := appID(w, r)
	if !ok {
		return
	}
	var body struct {
		Name          string   `json:"name"`
		Scopes        []string `json:"scopes"`
		ExpiresInDays int      `json:"expires_in_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Send the token's name and scopes as JSON."})
		return
	}
	if body.ExpiresInDays < 0 || body.ExpiresInDays > 366 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "A token lasts between 1 and 366 days; it can be rotated before then."})
		return
	}
	made, err := appBusiness.MintToken(r.Context(), id, strings.TrimSpace(body.Name), body.Scopes, time.Duration(body.ExpiresInDays)*24*time.Hour)
	if err != nil {
		refuse(w, r, err)
		return
	}
	auditBusiness.Record(r, "app.token_created", auditBusiness.CategoryApp, "Made a token for an app",
		map[string]interface{}{"app_id": id.String(), "token_id": made.Token.Id.String(), "scopes": viewOf(made.Token).Scopes})
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"data": map[string]any{"token": viewOf(made.Token), "plaintext": made.Plaintext}})
}

func tokenID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "tokenId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a token's id."})
		return uuid.Nil, false
	}
	return id, true
}

// RotateToken is POST /admin/apps/{appId}/tokens/{tokenId}/rotate: a new
// secret, in this answer only; the old one stops working at once.
func RotateToken(w http.ResponseWriter, r *http.Request) {
	app, ok := appID(w, r)
	if !ok {
		return
	}
	tok, ok := tokenID(w, r)
	if !ok {
		return
	}
	plaintext, err := appBusiness.RotateToken(r.Context(), app, tok)
	if err != nil {
		refuse(w, r, err)
		return
	}
	auditBusiness.Record(r, "app.token_rotated", auditBusiness.CategoryApp, "Rotated an app's token",
		map[string]interface{}{"app_id": app.String(), "token_id": tok.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"plaintext": plaintext}})
}

// RevokeToken is DELETE /admin/apps/{appId}/tokens/{tokenId}.
func RevokeToken(w http.ResponseWriter, r *http.Request) {
	app, ok := appID(w, r)
	if !ok {
		return
	}
	tok, ok := tokenID(w, r)
	if !ok {
		return
	}
	if err := appBusiness.RevokeToken(r.Context(), app, tok); err != nil {
		refuse(w, r, err)
		return
	}
	auditBusiness.Record(r, "app.token_revoked", auditBusiness.CategoryApp, "Revoked an app's token",
		map[string]interface{}{"app_id": app.String(), "token_id": tok.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Revoked: it stops working now."})
}
