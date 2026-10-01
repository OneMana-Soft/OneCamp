package Integration

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	business "github.com/akashc777/OneCamp/business/Integration"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/User"
)

func GetGoogleCalendarAuthUrl(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(model.UserInfo)
	if !ok {
		helpers.LogErrorWithContext(ctx, "controllers/GetGoogleCalendarAuthUrl Failed getting user from context")
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}

	url := business.GenerateGoogleCalendarAuthURL(ctx, &userInfo)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Generated Google Calendar Auth URL",
		"data": map[string]string{
			"url": url,
		},
	})
}

func GoogleCalendarCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var input struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}

	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/GoogleCalendarCallback Failed to parse the body of the req err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(model.UserInfo)
	if !ok {
		helpers.LogErrorWithContext(ctx, "controllers/GoogleCalendarCallback Failed getting user from context")
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}

	// Verify state matches user UUID
	if input.State != userInfo.UserDgraphInfo.Uuid {
		helpers.LogErrorWithContext(ctx, "controllers/GoogleCalendarCallback Invalid state parameter")
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid state"})
		return
	}

	err = business.ExchangeGoogleCalendarCodeAndSave(ctx, input.Code, &userInfo)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to authenticate Google Calendar", "err": err})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Successfully linked Google Calendar!"})
}

func GoogleCalendarCallbackGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(model.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}

	if state != userInfo.UserDgraphInfo.Uuid {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid state"})
		return
	}

	err := business.ExchangeGoogleCalendarCodeAndSave(ctx, code, &userInfo)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to authenticate Google Calendar", "err": err})
		return
	}

	frontendDomain := os.Getenv("FRONTEND_DOMAIN")
	if frontendDomain == "" {
		frontendDomain = os.Getenv("FE_HOST_DOMAIN")
	}
	if frontendDomain == "" {
		frontendDomain = "localhost:3001"
	}

	protocol := "https://"
	if strings.Contains(frontendDomain, "localhost") || strings.Contains(frontendDomain, "127.0.0.1") || strings.ToLower(os.Getenv("COOKIE_SECURE")) == "false" {
		protocol = "http://"
	}

	redirectUrl := protocol + frontendDomain + "/app/calendar"
	http.Redirect(w, r, redirectUrl, http.StatusFound)
}

func GetGoogleCalendarStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(model.UserInfo)
	if !ok {
		helpers.LogErrorWithContext(ctx, "controllers/GetGoogleCalendarStatus Failed getting user from context")
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}

	isConnected, err := business.IsGoogleCalendarConnected(ctx, &userInfo)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to check status", "err": err})
		return
	}

	// We also need to fetch the TaskSyncEnabled flag.
	// I'll update business.IsGoogleCalendarConnected to return more info or add a new business function.
	// For now, I'll fetch it from the domain directly or assume business has it.

	taskSyncEnabled := false
	integration, err := business.GetGoogleCalendarIntegration(ctx, &userInfo)
	if err == nil && integration != nil {
		taskSyncEnabled = integration.TaskSyncEnabled
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "Fetched Google Calendar status",
		"data": map[string]interface{}{
			"isConnected":     isConnected,
			"taskSyncEnabled": taskSyncEnabled,
		},
	})
}

func UpdateGoogleCalendarSyncTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(model.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}

	var input struct {
		Enabled bool `json:"enabled"`
	}
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid input"})
		return
	}

	err = business.UpdateGoogleCalendarSyncTask(ctx, input.Enabled, &userInfo)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to update sync preference", "err": err})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated task sync preference"})
}

func UnlinkGoogleCalendar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(model.UserInfo)
	if !ok {
		helpers.LogErrorWithContext(ctx, "controllers/UnlinkGoogleCalendar Failed getting user from context")
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}

	err := business.UnlinkGoogleCalendar(ctx, &userInfo)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to unlink Google Calendar", "err": err})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Successfully unlinked Google Calendar!"})
}

func HandleGoogleCalendarWebhook(w http.ResponseWriter, r *http.Request) {
	// Google posts notifications with no body (the headers carry the
	// channel id / token / resource id). We:
	//   1. Read and discard the body so the kernel doesn't drop the
	//      request prematurely once we return 200.
	//   2. Validate Channel-Token against the secret we registered
	//      with Google when the watch was created. Without this any
	//      reachable client could deliver fake "calendar changed"
	//      notifications.
	//   3. Spawn a detached goroutine with context.Background() so the
	//      sync work outlives the request handler. Cap with a 5-min
	//      timeout because the Calendar API call chain is bounded.
	channelID := r.Header.Get("X-Goog-Channel-ID")
	channelToken := r.Header.Get("X-Goog-Channel-Token")
	resourceState := r.Header.Get("X-Goog-Resource-State")
	resourceID := r.Header.Get("X-Goog-Resource-ID")

	// Drain + close so the connection can be reused.
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}

	if expected := os.Getenv("GOOGLE_CALENDAR_WEBHOOK_TOKEN"); expected != "" {
		if subtle.ConstantTimeCompare([]byte(channelToken), []byte(expected)) != 1 {
			helpers.LogWarnWithContext(r.Context(),
				"controllers/HandleGoogleCalendarWebhook bad channel token (chan=%s)", channelID)
			http.Error(w, "invalid channel token", http.StatusUnauthorized)
			return
		}
	}

	// Sync notification (the very first delivery) carries no work; ack.
	if resourceState == "sync" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Spawn the actual processing on a detached context so the request
	// goroutine returns immediately. Recover panics so a buggy parser
	// can't crash the process. helpers.GoSafe wraps both.
	helpers.GoSafe(func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_ = business.HandleGoogleCalendarWebhookEvent(bgCtx, channelID, channelToken, resourceState, resourceID)
	})

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Webhook received"})
}
