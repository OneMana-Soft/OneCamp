package controllers

// Admin management of SCIM provisioning credentials: /admin/scim/tokens.
//
// SESSION-AUTHENTICATED AND ADMIN-ONLY, unlike the /scim/v2 surface these credentials unlock. The two
// must not share auth, and the reason is worth stating plainly: a credential that could mint another
// credential of its own kind would make revocation meaningless, because whoever holds a leaked one could
// issue a replacement before the original was pulled.

import (
	"encoding/json"
	"net/http"
	"strings"

	scimBusiness "github.com/akashc777/OneCamp/business/Scim"
	"github.com/akashc777/OneCamp/helpers"
	scimModel "github.com/akashc777/OneCamp/models/postgres/Scim"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// CreateScimTokenHandler handles POST /admin/scim/tokens.
//
// The plaintext is in this response and in no other, ever. Only its hash is stored, so the UI has to
// present it as something to copy now rather than as a confirmation to dismiss.
func CreateScimTokenHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var body struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req", "status": "failed",
		})
		return
	}

	created, err := scimBusiness.CreateScimToken(ctx, body.Name, body.ExpiresInDays, userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/CreateScimTokenHandler failed: %+v", err)
		// err.Error() in msg, not in an err key: these are authored sentences from the business layer
		// ("name is required"), which is the actionable half of the response.
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": err.Error(), "status": "failed",
		})
		return
	}

	helpers.LogInfoWithContext(ctx, "controllers/CreateScimTokenHandler admin %s created SCIM credential %s",
		userInfo.UserPostgresInfo.Id, created.Token.Id)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"msg":    "Copy this credential now — it is not shown again.",
		"data": map[string]any{
			"token":     created.Token,
			"plaintext": created.Plaintext,
		},
	})
}

// ListScimTokensHandler handles GET /admin/scim/tokens. Never returns a secret.
func ListScimTokensHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	tokens, err := scimBusiness.ListScimTokens(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ListScimTokensHandler failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "could not read SCIM credentials", "status": "failed",
		})
		return
	}
	if tokens == nil {
		// A JSON [] rather than null. A nil slice marshals to null, which a client has to null-check
		// before it can iterate — so "no credentials yet" and "the field is missing" would look the same
		// on the wire for no reason.
		tokens = []*scimModel.ScimToken{}
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"data":   map[string]any{"tokens": tokens},
	})
}

// RevokeScimTokenHandler handles POST /admin/scim/tokens/{id}/revoke.
func RevokeScimTokenHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	tokenID, err := uuid.Parse(strings.TrimSpace(chi.URLParam(r, "id")))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the credential id", "status": "failed",
		})
		return
	}

	if err := scimBusiness.RevokeScimToken(ctx, tokenID); err != nil {
		// 404 for "nothing live to revoke", which covers both an unknown id and one already revoked.
		// Reported the same way because the caller's next action is identical and the distinction would
		// only confirm which ids exist.
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{
			"msg": "no live SCIM credential with that id", "status": "failed",
		})
		return
	}

	helpers.LogInfoWithContext(ctx, "controllers/RevokeScimTokenHandler admin %s revoked SCIM credential %s",
		userInfo.UserPostgresInfo.Id, tokenID)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"msg":    "SCIM credential revoked. The directory will stop being able to provision immediately.",
	})
}
