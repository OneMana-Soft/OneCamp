// Package controllers wires the email-notification HTTP surface area:
// per-user preferences, public unsubscribe, and the Resend webhook for
// bounce/complaint handling.
package controllers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	prefDomain "github.com/akashc777/OneCamp/domain/UserNotificationPreference"
	"github.com/akashc777/OneCamp/helpers"
	emailLogModels "github.com/akashc777/OneCamp/models/postgres/NotificationEmailLog"
	suppressionModels "github.com/akashc777/OneCamp/models/postgres/NotificationEmailSuppression"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	authService "github.com/akashc777/OneCamp/services/Auth"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// GetMyNotificationPreferences returns the current user's preferences,
// creating defaults if none exist.
//
// GET /user/notificationPreferences
func GetMyNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
			"msg":    "unauthorized",
			"status": "failed",
		})
		return
	}

	pref, err := prefDomain.LoadOrCreate(ctx, userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/Notification/GetMyNotificationPreferences err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "failed to load preferences",
			"status": "failed",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"data": map[string]any{
			"email_supported":            emailService.IsEmailEnabled(),
			"email_enabled":              pref.EmailEnabled,
			"email_mentions":             pref.EmailMentions,
			"email_dms":                  pref.EmailDMs,
			"email_task_assigned":        pref.EmailTaskAssigned,
			"email_task_status":          pref.EmailTaskStatus,
			"email_comments":             pref.EmailComments,
			"email_calls":                pref.EmailCalls,
			"email_channel_invites":      pref.EmailChannelInvites,
			"email_only_when_offline":    pref.EmailOnlyWhenOffline,
			"email_digest_frequency":     pref.EmailDigestFrequency,
			"quiet_hours_enabled":        pref.QuietHoursEnabled,
			"quiet_hours_start":          pref.QuietHoursStart,
			"quiet_hours_end":            pref.QuietHoursEnd,
			"quiet_hours_tz":             pref.QuietHoursTZ,
			"notifications_paused_until": pausedUntil(pref.NotificationsPausedUntil),
			// Focus time from the calendar: separate, because resuming a pause
			// does not end it; the event does.
			"focus_until": pausedUntil(pref.FocusUntil),
			// Read receipts in DMs and group chats: the person's own choice,
			// and whether the workspace allows them at all.
			"read_receipts":         pref.ReadReceipts,
			"read_receipts_allowed": settingsBusiness.ReadReceiptsEnabled(),
		},
	})
}

// pausedUntil is a pause still in force, or nil: an expired pause is not news.
func pausedUntil(t *time.Time) *time.Time {
	if t == nil || !t.After(time.Now()) {
		return nil
	}
	return t
}

type pauseInput struct {
	// Until pauses to a moment; Minutes pauses for a while; neither resumes.
	Until   *time.Time `json:"until,omitempty"`
	Minutes int        `json:"minutes,omitempty"`
}

// PauseMyNotifications pauses (or resumes) every notification to the person's
// devices: Slack's "pause notifications". Quiet hours keep working beside it.
// POST /user/notificationPause {until?|minutes?}
func PauseMyNotifications(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not signed in"})
		return
	}
	// Preferences are keyed by the Postgres user id, as everywhere else here.
	userID := userInfo.UserPostgresInfo.Id
	var in pauseInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	until, err := pauseEnd(in, time.Now())
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	pref, err := prefDomain.SetPause(ctx, userID, until)
	var pe *prefDomain.PauseError
	if errors.As(err, &pe) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": pe.Error()})
		return
	}
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't change your notifications. Try again."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]any{
		"notifications_paused_until": pausedUntil(pref.NotificationsPausedUntil),
		"focus_until":                pausedUntil(pref.FocusUntil),
	}})
}

// pauseEnd turns a request into the moment the pause ends (nil = resume).
func pauseEnd(in pauseInput, now time.Time) (*time.Time, error) {
	var until time.Time
	switch {
	case in.Until != nil:
		until = *in.Until
	case in.Minutes > 0:
		until = now.Add(time.Duration(in.Minutes) * time.Minute)
	default:
		return nil, nil
	}
	if err := prefDomain.CheckPauseEnd(until, now); err != nil {
		return nil, err
	}
	return &until, nil
}

// UpdateMyNotificationPreferences applies a partial update.
// POST /user/notificationPreferences
func UpdateMyNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
			"msg":    "unauthorized",
			"status": "failed",
		})
		return
	}

	var input prefModels.UpdatePreferenceInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid request body",
			"status": "failed",
		})
		return
	}

	// Validate digest enum and HH:MM strings.
	if input.EmailDigestFrequency != nil {
		v := *input.EmailDigestFrequency
		if v != "off" && v != "daily" && v != "weekly" {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg":    "invalid digest frequency",
				"status": "failed",
			})
			return
		}
	}
	if input.QuietHoursStart != nil && !validHHMM(*input.QuietHoursStart) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid quiet_hours_start (HH:MM)",
			"status": "failed",
		})
		return
	}
	if input.QuietHoursEnd != nil && !validHHMM(*input.QuietHoursEnd) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid quiet_hours_end (HH:MM)",
			"status": "failed",
		})
		return
	}
	if input.QuietHoursTZ != nil && *input.QuietHoursTZ != "" {
		known, err := prefModels.KnownTimeZone(*input.QuietHoursTZ)
		if _, lerr := time.LoadLocation(*input.QuietHoursTZ); err != nil || lerr != nil || !known {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg":    "invalid quiet_hours_tz (must be IANA TZ)",
				"status": "failed",
			})
			return
		}
	}

	// Make sure a row exists, then apply the update.
	if _, err := prefDomain.LoadOrCreate(ctx, userInfo.UserPostgresInfo.Id); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "failed to load preferences",
			"status": "failed",
		})
		return
	}
	if err := prefDomain.Update(ctx, userInfo.UserPostgresInfo.Id, input); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "failed to update preferences",
			"status": "failed",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"msg":    "preferences updated",
	})
}

// PublicUnsubscribe handles the one-click unsubscribe flow.
//
// Two callers, two response shapes:
//
//  1. Mailbox provider one-click: POST with no Accept header (or */*).
//     Returns 200 JSON. Per RFC 8058, the provider only cares that the
//     response is a 2xx.
//
//  2. Human clicking the footer link in an email: GET with
//     "Accept: text/html". After flipping email_enabled to false we
//     302-redirect to the FE confirmation page.
//
// Tokens that don't resolve always get a generic success / redirect: never
// leak presence info.
//
// GET / POST /public/notifications/unsubscribe?token=...
func PublicUnsubscribe(w http.ResponseWriter, r *http.Request) {
	publicSetEmailEnabled(w, r, false, "you've been unsubscribed")
}

// PublicResubscribe is the inverse of PublicUnsubscribe. Reuses the same
// token (we don't rotate tokens on unsubscribe — they're symmetric per-user
// identifiers, not single-use tokens) and flips email_enabled back to true.
//
// We deliberately do NOT expose this to mailbox providers' one-click flow:
// resubscribe is always a deliberate human action. Both GET and POST are
// supported so the FE confirmation page can offer a "resubscribe" button.
//
// GET / POST /public/notifications/resubscribe?token=...
func PublicResubscribe(w http.ResponseWriter, r *http.Request) {
	publicSetEmailEnabled(w, r, true, "you're resubscribed")
}

// publicSetEmailEnabled is the shared body of the two public endpoints.
// Both surfaces (machine one-click, human browser click) are handled here
// so the auth, error, and redirect logic stays in one place.
//
// The function:
//
//  1. Validates the token and looks up the preferences row.
//  2. Skips the DB write when the row is already in the desired state
//     (avoids unnecessary updated_at churn from idempotent retries).
//  3. For resubscribe, checks the suppression list. A user whose email
//     is hard-bouncing or on a complaint list can flip email_enabled
//     back to true, but no email will actually be delivered until the
//     workspace admin removes the suppression. We surface that to the
//     FE via &warn=suppressed so the page can show a clear notice.
//  4. Returns JSON for machines and 302 for browsers.
func publicSetEmailEnabled(w http.ResponseWriter, r *http.Request, enabled bool, successMsg string) {
	ctx := r.Context()
	token := strings.TrimSpace(r.URL.Query().Get("token"))

	wantsHTML := r.Method == http.MethodGet &&
		strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")

	// Status query param tells the FE landing page which copy to render.
	// On unsubscribe success we forward the token so the FE can offer an
	// "I changed my mind, resubscribe" button without a fresh email round-trip.
	// On resubscribe success the token is omitted — no further action needed.
	status := "unsubscribed"
	if enabled {
		status = "resubscribed"
	}
	feLanding := authService.FrontendBaseURL() + "/unsubscribe?status=" + status

	if token == "" {
		if wantsHTML {
			http.Redirect(w, r, feLanding+"&missing=1", http.StatusFound)
			return
		}
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "missing token",
			"status": "failed",
		})
		return
	}

	// Build the success-redirect URL once, so all branches use the same value.
	// On the unsubscribe path we forward the token so the FE can offer the
	// resubscribe CTA. On the resubscribe path we don't.
	successRedirect := feLanding
	if !enabled {
		successRedirect = feLanding + "&token=" + url.QueryEscape(token)
	}

	pref, err := prefDomain.GetByUnsubscribeToken(ctx, token)
	if err != nil {
		// Internal error — but for one-click POST we still return 200 so
		// providers don't keep retrying us. Log + move on.
		helpers.LogErrorWithContext(ctx,
			"controllers/Notification/publicSetEmailEnabled lookup err: %+v", err)
		if wantsHTML {
			http.Redirect(w, r, successRedirect, http.StatusFound)
			return
		}
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
			"status": "success",
			"msg":    successMsg,
		})
		return
	}

	// Token unknown — always show success to the user (token-presence-safe).
	if pref == nil {
		if wantsHTML {
			http.Redirect(w, r, successRedirect, http.StatusFound)
			return
		}
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
			"status": "success",
			"msg":    successMsg,
		})
		return
	}

	// On resubscribe, check the suppression list. We still flip the
	// preference flag so the user's recorded intent is correct, but we
	// surface a warning so the FE can tell them their address is currently
	// blocked at the provider level (hard bounce / spam complaint). The
	// workspace admin has to clear the suppression manually before mail
	// will actually flow again.
	suppressedReason := ""
	if enabled {
		if user, uerr := userDomain.GetActiveUserByUUID(ctx, pref.UserID); uerr == nil && user != nil && user.EmailID != "" {
			if reason, _ := suppressionModels.IsSuppressed(user.EmailID); reason != "" {
				suppressedReason = reason
			}
		}
	}

	// Skip the DB write when the row is already in the desired state.
	// Idempotent retries should not bump updated_at or invalidate caches.
	if pref.EmailEnabled == enabled {
		if wantsHTML {
			redir := successRedirect
			if suppressedReason != "" {
				redir += "&warn=suppressed"
			}
			http.Redirect(w, r, redir, http.StatusFound)
			return
		}
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
			"status": "success",
			"msg":    successMsg,
		})
		return
	}

	if err := prefDomain.SetEmailEnabledByToken(ctx, token, enabled); err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/Notification/publicSetEmailEnabled update err: %+v", err)
		if wantsHTML {
			http.Redirect(w, r, successRedirect, http.StatusFound)
			return
		}
		// Same reasoning: 200 to satisfy one-click POST contract.
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
			"status": "success",
			"msg":    successMsg,
		})
		return
	}

	helpers.LogInfoWithContext(ctx,
		"controllers/Notification/publicSetEmailEnabled user=%s enabled=%t",
		pref.UserID.String(), enabled)

	if wantsHTML {
		redir := successRedirect
		if suppressedReason != "" {
			redir += "&warn=suppressed"
		}
		http.Redirect(w, r, redir, http.StatusFound)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"msg":    successMsg,
	})
}

// ResendWebhook receives delivery events from Resend (delivered, bounced,
// complained) and updates the suppression list and email log accordingly.
//
// Authentication: Svix-style signature verification when
// RESEND_WEBHOOK_SECRET is set. Resend uses Svix under the hood, so the
// signed input is `<svix_id>.<svix_timestamp>.<body>` and the resulting
// HMAC-SHA256 is base64-encoded and prefixed with "v1,". The secret value
// is shaped "whsec_<base64-bytes>" — we strip the prefix and base64-decode
// to get the actual HMAC key. We also reject signatures whose timestamp
// is older than 5 minutes to prevent replay attacks.
//
// POST /webhooks/resend
func ResendWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "could not read body",
			"status": "failed",
		})
		return
	}

	secret := strings.TrimSpace(os.Getenv("RESEND_WEBHOOK_SECRET"))
	if secret != "" {
		svixID := r.Header.Get("Svix-Id")
		svixTimestamp := r.Header.Get("Svix-Timestamp")
		svixSignature := r.Header.Get("Svix-Signature")
		if !verifyResendWebhookSignature(secret, svixID, svixTimestamp, svixSignature, body) {
			helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
				"msg":    "invalid signature",
				"status": "failed",
			})
			return
		}
	}

	var payload struct {
		Type string `json:"type"`
		Data struct {
			EmailID string   `json:"email_id"`
			To      []string `json:"to"`
			Reason  string   `json:"reason"`
			Bounce  struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"bounce"`
			Complaint struct {
				Type string `json:"type"`
			} `json:"complaint"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid json",
			"status": "failed",
		})
		return
	}

	// Map the event to our internal status. Only hard (Permanent) bounces
	// and complaints are added to the suppression list. Soft / Transient /
	// Undetermined bounces are recorded in the log but the address is not
	// suppressed — Resend recommends keeping retrying transient failures.
	//
	// Resend sends bounce.type values: "Permanent" / "Transient" / "Undetermined".
	// Older payloads used "hard" / "soft"; we accept both for safety.
	switch payload.Type {
	case "email.bounced":
		bt := strings.ToLower(strings.TrimSpace(payload.Data.Bounce.Type))
		isHardBounce := bt == "permanent" || bt == "hard" || bt == "block"
		if isHardBounce {
			for _, addr := range payload.Data.To {
				_ = suppressionModels.Upsert(addr, suppressionModels.ReasonBounced, payload.Data.Bounce.Message)
			}
		}
		if payload.Data.EmailID != "" {
			_ = emailLogModels.UpdateStatusByProviderID(payload.Data.EmailID,
				emailLogModels.StatusBounced, payload.Data.Bounce.Message)
		}
	case "email.complained":
		for _, addr := range payload.Data.To {
			_ = suppressionModels.Upsert(addr, suppressionModels.ReasonComplained, payload.Data.Complaint.Type)
		}
		if payload.Data.EmailID != "" {
			_ = emailLogModels.UpdateStatusByProviderID(payload.Data.EmailID,
				emailLogModels.StatusComplained, payload.Data.Complaint.Type)
		}
	default:
		// Other event types (delivered, delivery_delayed, opened, clicked) are
		// safe to ignore for now. They could feed analytics later.
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "ok"})
}

// verifyResendWebhookSignature validates a Svix-style webhook signature.
//
// Inputs:
//
//   - secret: the configured webhook secret. Resend gives it shaped like
//     "whsec_<base64-bytes>". We strip the prefix and base64-decode to get
//     the raw HMAC key.
//   - svixID / svixTimestamp / svixSignature: the three Svix-* request
//     headers. svixSignature is space-separated; each token is "v1,<base64>"
//     where base64 is HMAC-SHA256(key, "id.timestamp.body").
//
// We reject:
//
//   - missing headers
//   - timestamps more than 5 minutes from now (replay protection)
//   - any signature where no token matches in constant time
func verifyResendWebhookSignature(secret, svixID, svixTimestamp, svixSignature string, body []byte) bool {
	if svixID == "" || svixTimestamp == "" || svixSignature == "" {
		return false
	}

	// Replay window: tolerate ±5 minutes.
	tsUnix, err := strconv.ParseInt(strings.TrimSpace(svixTimestamp), 10, 64)
	if err != nil {
		return false
	}
	skew := time.Since(time.Unix(tsUnix, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > 5*time.Minute {
		return false
	}

	// Resolve the HMAC key: strip "whsec_" prefix if present, then base64-decode.
	rawSecret := strings.TrimPrefix(secret, "whsec_")
	key, err := base64.StdEncoding.DecodeString(rawSecret)
	if err != nil {
		// Allow the secret to be supplied as a raw string for tests / non-Svix
		// integrations. This makes the verifier a strict superset of the
		// previous behaviour.
		key = []byte(rawSecret)
	}

	signed := svixID + "." + svixTimestamp + "."
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signed))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	// Svix sends one or more space-separated "v1,<sig>" tokens; we accept
	// any match.
	for _, tok := range strings.Fields(svixSignature) {
		parts := strings.SplitN(tok, ",", 2)
		if len(parts) != 2 || parts[0] != "v1" {
			continue
		}
		if hmac.Equal([]byte(parts[1]), []byte(expected)) {
			return true
		}
	}
	return false
}

func validHHMM(s string) bool {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return false
	}
	if len(parts[0]) != 2 || len(parts[1]) != 2 {
		return false
	}
	h, m := 0, 0
	for _, c := range parts[0] {
		if c < '0' || c > '9' {
			return false
		}
		h = h*10 + int(c-'0')
	}
	for _, c := range parts[1] {
		if c < '0' || c > '9' {
			return false
		}
		m = m*10 + int(c-'0')
	}
	return h < 24 && m < 60
}

// TeammateNotificationStatus says whether a teammate's notifications are held
// (paused, or quiet hours) and until when, so a DM can say so.
// GET /user/notificationStatus/{user_uuid}
func TeammateNotificationStatus(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "user_uuid"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a person."})
		return
	}
	status, err := notificationBusiness.HeldStatusOf(id, time.Now())
	if err != nil {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't check. Try again."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": status})
}

// NotifyAnyway sends one urgent ping through a teammate's pause or quiet
// hours, once a day, in a DM. POST /user/notifyAnyway {user_uuid}
func NotifyAnyway(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not signed in"})
		return
	}
	var in struct {
		UserUUID string `json:"user_uuid"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Couldn't read that request."})
		return
	}
	to, err := uuid.Parse(in.UserUUID)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a person."})
		return
	}
	err = notificationBusiness.NotifyAnyway(ctx, &userInfo, to, time.Now())
	var ue *notificationBusiness.UrgentError
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Notified"})
	case errors.As(err, &ue):
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": ue.Msg})
	default:
		helpers.LogErrorWithContext(ctx, "controllers/NotifyAnyway err: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't notify them. Try again."})
	}
}
