package controllers

// HTTP surface for two-factor authentication.
//
// Four authenticated endpoints for managing your own second factor, and one unauthenticated endpoint
// that completes a login challenge. The challenge endpoint is the only one that can mint a session, and
// it can only do so for a user who already passed the password step — see business/Auth.IssueTOTPChallenge.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	adapter "github.com/akashc777/OneCamp/adapter/User"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	authBusiness "github.com/akashc777/OneCamp/business/Auth"
	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// GetTwoFactorStatus handles GET /auth/2fa
//
// Drives the settings screen. Reports whether a factor is live, whether an enrolment was started and
// abandoned, and how many recovery codes remain — the last so a user can notice they are low BEFORE the
// one occasion they need one.
func GetTwoFactorStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	status, err := authBusiness.TOTPStatusFor(ctx, userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GetTwoFactorStatus failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "could not read two-factor status", "err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"data": map[string]any{
			"enrolled":              status.Enrolled,
			"pending_enrolment":     status.PendingEnrolment,
			"unused_recovery_codes": status.UnusedRecoveryCodes,
		},
	})
}

// BeginTwoFactorSetup handles POST /auth/2fa/setup
//
// Returns a secret and an otpauth:// URI. NOTHING about the account changes yet: the enrolment is stored
// unconfirmed, so abandoning this screen cannot lock anyone out.
func BeginTwoFactorSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	enrollment, err := authBusiness.BeginTOTPEnrollment(ctx, userInfo.UserPostgresInfo.Id, userInfo.UserPostgresInfo.EmailID)
	if err != nil {
		// A MISSING KEK IS AN OPERATOR CONDITION, NOT A USER ERROR. 409 with the actionable sentence in
		// msg, matching how an unreadable AI provider key is reported: the person clicking "enable" can
		// do nothing about TOTP_KEK, and a 500 would send them to support instead of the operator.
		if errors.Is(err, models.ErrTOTPKEKMissing) {
			helpers.LogErrorWithContext(ctx, "controllers/BeginTwoFactorSetup refused: %v", err)
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"msg":  err.Error(),
				"code": "totp_kek_missing",
			})
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/BeginTwoFactorSetup failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "could not start two-factor setup", "err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"data": map[string]any{
			"secret": enrollment.Secret,
			"uri":    enrollment.URI,
		},
	})
}

// ConfirmTwoFactorSetup handles POST /auth/2fa/confirm
//
// Verifies the first code and returns the recovery codes. Those codes are shown HERE AND NOWHERE ELSE —
// only their hashes are kept — so the response is the single opportunity the user has to save them.
func ConfirmTwoFactorSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Code == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "a code is required"})
		return
	}

	codes, err := authBusiness.ConfirmTOTPEnrollment(ctx, userInfo.UserPostgresInfo.Id, body.Code)
	if err != nil {
		if errors.Is(err, authBusiness.ErrTOTPCodeInvalid) {
			// 400 rather than 401: the caller IS authenticated, they simply typed the wrong digits.
			// Logged at INFO because it is the caller's state, not a server fault, and mistyping a
			// six-digit code during setup is expected rather than exceptional.
			helpers.LogInfoWithContext(ctx, "controllers/ConfirmTwoFactorSetup rejected an invalid code")
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": err.Error(), "code": "totp_code_invalid",
			})
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/ConfirmTwoFactorSetup failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "could not enable two-factor authentication", "err": err,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"msg":    "Two-factor authentication is on. Save these recovery codes now — they are not shown again.",
		"data":   map[string]any{"recovery_codes": codes},
	})
}

// DisableTwoFactor handles POST /auth/2fa/disable
//
// Requires a valid code. Turning the factor off is exactly what an attacker holding a stolen session
// would do first, so the request has to prove possession of the factor it is removing.
func DisableTwoFactor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Code == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "enter a code from your authenticator app, or a recovery code, to turn this off",
		})
		return
	}

	if err := authBusiness.DisableTOTP(ctx, userInfo.UserPostgresInfo.Id, body.Code); err != nil {
		switch {
		case errors.Is(err, authBusiness.ErrTOTPCodeInvalid):
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": err.Error(), "code": "totp_code_invalid",
			})
		case errors.Is(err, authBusiness.ErrTOTPNotEnrolled):
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg": err.Error(), "code": "totp_not_enrolled",
			})
		case errors.Is(err, models.ErrTOTPSecretUnreadable):
			// The user cannot produce a valid code for a secret nobody can decrypt, so a recovery code
			// is their only route and an admin reset is the other. Said plainly rather than reported as
			// a wrong code.
			helpers.LogErrorWithContext(ctx, "controllers/DisableTwoFactor: %v", err)
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"msg": err.Error(), "code": "totp_secret_unreadable",
			})
		default:
			helpers.LogErrorWithContext(ctx, "controllers/DisableTwoFactor failed: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
				"msg": "could not turn off two-factor authentication", "err": err,
			})
		}
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"msg":    "Two-factor authentication is off.",
	})
}

// AdminResetTwoFactor handles POST /admin/auth/2fa/reset
//
// The break-glass counterpart to DisableTwoFactor above. That endpoint demands a code, which is correct
// and leaves two dead ends it cannot help with: a user who has lost both their phone and their recovery
// codes, and — collectively — every enrolled user at once if TOTP_KEK is replaced, because their stored
// secrets stop decrypting and no code they can produce will verify again.
//
// ADMIN-ONLY BY ROUTE, not by a check in here: it is registered on adminUserRouter, which is mounted
// behind CSRFMiddleware + VerifyAuth + VerifyAdminAuthOnlyPostgres. Self-targeting is refused in the
// business layer, because allowing it would turn this into a bypass of the code requirement for anyone
// holding a stolen admin session.
//
// ALWAYS AUDITED, including the refusals. Removing somebody's second factor is precisely the action an
// attacker who reached an admin account would want, so the question "who did this, to whom, and when"
// has to be answerable afterwards.
func AdminResetTwoFactor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)

	var body adapter.InputUserUUID
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req", "status": "failed",
		})
		return
	}

	targetID, err := uuid.Parse(strings.TrimSpace(body.UserUuid))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse userUUID in req", "status": "failed",
		})
		return
	}

	// The target must exist before anything is recorded or cleared. Without this a typo'd uuid would
	// write an audit row naming a user who was never touched, which is worse than no row: it puts a
	// security event that did not happen into the evidence an auditor relies on.
	target, err := domain.GetUserByUUID(ctx, targetID)
	if err != nil || target == nil || target.Id != targetID {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{
			"msg": "no such user", "status": "failed",
		})
		return
	}

	wasEnrolled, err := authBusiness.AdminResetTOTP(ctx, actor.UserPostgresInfo.Id, targetID)
	if err != nil {
		if errors.Is(err, authBusiness.ErrTOTPSelfResetRefused) {
			// Recorded as an ATTEMPT. A refused self-reset is a person trying to remove their own second
			// factor without proving possession of it, which is worth seeing in the log whether it
			// succeeded or not.
			auditBusiness.Record(r, "auth.2fa.reset_refused", auditBusiness.CategorySecurity,
				"Refused a self-targeted two-factor reset for "+target.EmailID,
				map[string]interface{}{"target_user_id": targetID.String(), "reason": "self_target"})
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
				"msg": err.Error(), "status": "failed", "code": "totp_self_reset_refused",
			})
			return
		}
		helpers.LogErrorWithContext(ctx, "controllers/AdminResetTwoFactor failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "could not reset two-factor authentication", "status": "failed",
		})
		return
	}

	// Two different events, named differently on purpose. A no-op on an account that never had a factor
	// is not a security change, and logging it as one would inflate the trail with events that did not
	// occur — the opposite of what an audit log is for.
	if wasEnrolled {
		auditBusiness.Record(r, "auth.2fa.reset", auditBusiness.CategorySecurity,
			"Reset two-factor authentication for "+target.EmailID,
			map[string]interface{}{"target_user_id": targetID.String()})
	} else {
		auditBusiness.Record(r, "auth.2fa.reset_noop", auditBusiness.CategorySecurity,
			"Two-factor reset requested for "+target.EmailID+", which had none enabled",
			map[string]interface{}{"target_user_id": targetID.String()})
	}

	msg := "Two-factor authentication has been reset. " +
		"They can sign in with their password and enrol a new device."
	if !wasEnrolled {
		msg = "That account did not have two-factor authentication enabled. Nothing was changed."
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"msg":    msg,
		"data":   map[string]any{"was_enrolled": wasEnrolled},
	})
}

// TOTPLogin handles POST /auth/login/totp — the second half of a two-step sign-in.
//
// UNAUTHENTICATED by necessity: the caller has no session yet, which is the whole point. What it does
// have is a challenge proving the password step succeeded, and that challenge is the only thing standing
// between this endpoint and an unauthenticated session mint. It is verified before anything else happens.
//
// Rate limited at the route, because this is the one place a six-digit secret can be guessed.
func TOTPLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var body struct {
		Challenge string `json:"challenge"`
		Code      string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "invalid request body", "status": "failed",
		})
		return
	}
	if body.Challenge == "" || body.Code == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "a challenge and a code are required", "status": "failed",
		})
		return
	}

	userID, err := authBusiness.ParseTOTPChallenge(body.Challenge)
	if err != nil {
		// 401 and the "start again" wording: an expired challenge is the common case by far, and the
		// user's next action is to re-enter their password.
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
			"msg": err.Error(), "status": "failed", "code": "totp_challenge_invalid",
		})
		return
	}

	if err := authBusiness.CompleteTOTPChallenge(ctx, userID, body.Code); err != nil {
		switch {
		case errors.Is(err, models.ErrTOTPSecretUnreadable):
			helpers.LogErrorWithContext(ctx, "controllers/TOTPLogin: %v", err)
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"msg": err.Error(), "status": "failed", "code": "totp_secret_unreadable",
			})
		case errors.Is(err, authBusiness.ErrTOTPCodeInvalid), errors.Is(err, authBusiness.ErrTOTPNotEnrolled):
			// INFO, not ERROR: a wrong code is the caller's state and recurs by design. Logging it at
			// ERROR would turn every fat-fingered login into an alert.
			helpers.LogInfoWithContext(ctx, "controllers/TOTPLogin rejected a code")
			helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
				"msg": err.Error(), "status": "failed", "code": "totp_code_invalid",
			})
		default:
			helpers.LogErrorWithContext(ctx, "controllers/TOTPLogin failed: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
				"msg": "could not complete sign-in", "err": err, "status": "failed",
			})
		}
		return
	}

	// Recorded HERE rather than at the password step, because this is the moment a sign-in actually
	// happened. A password that produced a challenge and was never followed by a code is not a login,
	// and counting it as one would make the audit trail overstate what took place.
	_ = domain.RecordLoginMethod(ctx, userID, models.AuthMethodEmail)

	// Only now does a session exist. issueAuthCookies writes the success body itself.
	issueAuthCookies(w, r, ctx, userID.String())
}
