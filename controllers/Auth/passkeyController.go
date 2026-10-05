package controllers

// Passkeys: adding them while signed in, and signing in with one. The
// ceremony logic lives in business/Passkey; this file is the HTTP surface.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	passkeyBusiness "github.com/akashc777/OneCamp/business/Passkey"
	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	passkeyModel "github.com/akashc777/OneCamp/models/postgres/Passkey"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// A WebAuthn answer is a few kilobytes; anything far larger is not one.
const maxCeremonyBody = 64 << 10

func passkeyFail(w http.ResponseWriter, r *http.Request, where string, err error) {
	var pe *passkeyBusiness.Error
	if errors.As(err, &pe) {
		status := http.StatusBadRequest
		if errors.Is(err, passkeyBusiness.ErrNotConfigured) {
			status = http.StatusServiceUnavailable
		}
		helpers.WriteJSON(w, status, helpers.Envolope{"msg": pe.Msg, "status": "failed"})
		return
	}
	helpers.LogErrorWithContext(r.Context(), "controllers/Auth/%s err: %+v", where, err)
	helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Passkeys aren't available right now. Try again.", "status": "failed"})
}

// PasskeyLoginBegin POST /auth/passkey/begin — public. The challenge for a
// sign-in with any passkey for this server.
func PasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	options, ceremony, err := passkeyBusiness.BeginLogin(r.Context())
	if err != nil {
		passkeyFail(w, r, "PasskeyLoginBegin", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"options": options, "ceremony": ceremony}})
}

// PasskeyLoginFinish POST /auth/passkey/finish?ceremony= — public. The body is
// the browser's answer; a good one signs the person in.
func PasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, err := passkeyBusiness.FinishLogin(ctx, r.URL.Query().Get("ceremony"), io.LimitReader(r.Body, maxCeremonyBody), time.Now())
	if err != nil {
		var pe *passkeyBusiness.Error
		if errors.As(err, &pe) {
			helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": pe.Msg, "status": "failed"})
			return
		}
		passkeyFail(w, r, "PasskeyLoginFinish", err)
		return
	}
	_ = domain.RecordLoginMethod(ctx, userID, models.AuthMethodPasskey)
	issueAuthCookies(w, r, ctx, userID.String())
}

func passkeyUser(r *http.Request) models.UserInfo {
	return r.Context().Value(helpers.UserInfoContextKey).(models.UserInfo)
}

// ListPasskeys GET /auth/passkeys — the signed-in person's passkeys.
func ListPasskeys(w http.ResponseWriter, r *http.Request) {
	keys, err := passkeyModel.List(passkeyUser(r).UserPostgresInfo.Id)
	if err != nil {
		passkeyFail(w, r, "ListPasskeys", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": keys})
}

// BeginPasskeyRegistration POST /auth/passkeys/begin — starts adding one.
func BeginPasskeyRegistration(w http.ResponseWriter, r *http.Request) {
	u := passkeyUser(r)
	// The demo is one account everyone shares; a passkey added to it would
	// sit in the next visitor's settings.
	if helpers.IsDemoVisitor(u.UserPostgresInfo.EmailID) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "The demo is shared by everyone who opens it, so it can't keep a passkey. Install OneCamp free to use passkeys with your own account."})
		return
	}
	options, ceremony, err := passkeyBusiness.BeginRegistration(r.Context(), u.UserPostgresInfo.Id)
	if err != nil {
		passkeyFail(w, r, "BeginPasskeyRegistration", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{"options": options, "ceremony": ceremony}})
}

// FinishPasskeyRegistration POST /auth/passkeys/finish?ceremony=&name= — the
// body is the browser's new credential.
func FinishPasskeyRegistration(w http.ResponseWriter, r *http.Request) {
	u := passkeyUser(r)
	q := r.URL.Query()
	key, err := passkeyBusiness.FinishRegistration(r.Context(), u.UserPostgresInfo.Id, q.Get("ceremony"), q.Get("name"), io.LimitReader(r.Body, maxCeremonyBody))
	if err != nil {
		passkeyFail(w, r, "FinishPasskeyRegistration", err)
		return
	}
	auditBusiness.Record(r, "auth.passkey.add", auditBusiness.CategorySecurity, "Added a passkey",
		map[string]interface{}{"passkey_id": key.Id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": key})
}

// RenamePasskey POST /auth/passkeys/{id}/rename {name}
func RenamePasskey(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a passkey."})
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	name, err := passkeyBusiness.PasskeyName(in.Name)
	if err != nil {
		passkeyFail(w, r, "RenamePasskey", err)
		return
	}
	ok, err := passkeyModel.Rename(id, passkeyUser(r).UserPostgresInfo.Id, name)
	if err != nil {
		passkeyFail(w, r, "RenamePasskey", err)
		return
	}
	if !ok {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That passkey isn't yours."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Renamed"})
}

// DeletePasskey POST /auth/passkeys/{id}/delete
func DeletePasskey(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a passkey."})
		return
	}
	ok, err := passkeyModel.Delete(id, passkeyUser(r).UserPostgresInfo.Id)
	if err != nil {
		passkeyFail(w, r, "DeletePasskey", err)
		return
	}
	if !ok {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "That passkey isn't yours."})
		return
	}
	auditBusiness.Record(r, "auth.passkey.remove", auditBusiness.CategorySecurity, "Removed a passkey",
		map[string]interface{}{"passkey_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Removed"})
}
