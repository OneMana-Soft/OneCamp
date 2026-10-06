package controllers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	rootBusiness "github.com/akashc777/OneCamp/business"
	business "github.com/akashc777/OneCamp/business/Webhook"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	"github.com/akashc777/OneCamp/helpers"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// HandleCreateWebhook creates a new webhook (admin only).
func HandleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(*userModel.UserInfo)
	if !ok || userInfo == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var input business.WebhookCreateInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid request body"})
		return
	}

	// Validation
	if input.Name == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Name is required", "field": "name"})
		return
	}
	if len(input.Name) > 120 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Name must be at most 120 characters", "field": "name"})
		return
	}
	if input.Type == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Type is required", "field": "type"})
		return
	}
	if input.Type != "incoming" && input.Type != "outgoing" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Type must be 'incoming' or 'outgoing'", "field": "type"})
		return
	}
	if len(input.BotName) > 64 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Bot name must be at most 64 characters", "field": "bot_name"})
		return
	}
	// Sanitize events: reject arrays containing empty strings to prevent FE crashes
	for i, ev := range input.Events {
		if ev == "" {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": fmt.Sprintf("Event at index %d cannot be empty", i), "field": "events"})
			return
		}
	}

	webhook, err := business.CreateWebhook(ctx, input, userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/HandleCreateWebhook Failed to create webhook err: %+v", err)
		// Distinguish validation errors from internal errors
		status := http.StatusInternalServerError
		if isWebhookValidationError(err) {
			status = http.StatusBadRequest
		}
		helpers.WriteJSON(w, status, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"webhook": webhook})
}

// HandleGetAllWebhooks returns all webhooks (admin only).
// Tokens are redacted from list responses to prevent leaks.
func HandleGetAllWebhooks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	webhooks, err := business.GetAllWebhooks(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/HandleGetAllWebhooks Failed to get webhooks err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": "Failed to retrieve webhooks"})
		return
	}

	// Redact tokens from list view
	for _, wh := range webhooks {
		wh.Token = ""
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"webhooks": webhooks})
}

// HandleGetWebhook returns a single webhook by ID (admin only).
func HandleGetWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	webhookIdStr := chi.URLParam(r, "webhookId")
	webhookId, err := uuid.Parse(webhookIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid webhook ID"})
		return
	}

	webhook, err := business.GetWebhook(ctx, webhookId)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": "Failed to retrieve webhook"})
		return
	}

	if webhook == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "Webhook not found"})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"webhook": webhook})
}

// HandleUpdateWebhook updates a webhook configuration (admin only).
func HandleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	webhookIdStr := chi.URLParam(r, "webhookId")
	webhookId, err := uuid.Parse(webhookIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid webhook ID"})
		return
	}

	var input business.WebhookUpdateInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid request body"})
		return
	}

	// Validation
	if input.Name == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Name is required", "field": "name"})
		return
	}
	if len(input.Name) > 120 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Name must be at most 120 characters", "field": "name"})
		return
	}
	if len(input.BotName) > 64 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Bot name must be at most 64 characters", "field": "bot_name"})
		return
	}
	for i, ev := range input.Events {
		if ev == "" {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": fmt.Sprintf("Event at index %d cannot be empty", i), "field": "events"})
			return
		}
	}

	webhook, err := business.UpdateWebhook(ctx, webhookId, input)
	if err != nil {
		status := http.StatusInternalServerError
		if err.Error() == "webhook not found" {
			status = http.StatusNotFound
		} else if isWebhookValidationError(err) {
			status = http.StatusBadRequest
		}
		helpers.WriteJSON(w, status, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"webhook": webhook})
}

// HandleDeleteWebhook soft-deletes a webhook (admin only).
func HandleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	webhookIdStr := chi.URLParam(r, "webhookId")
	webhookId, err := uuid.Parse(webhookIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid webhook ID"})
		return
	}

	err = business.DeleteWebhook(ctx, webhookId)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Webhook deleted successfully"})
}

// HandleRegenerateToken generates a new token for a webhook (admin only).
func HandleRegenerateToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	webhookIdStr := chi.URLParam(r, "webhookId")
	webhookId, err := uuid.Parse(webhookIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid webhook ID"})
		return
	}

	token, err := business.RegenerateToken(ctx, webhookId)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"token": token})
}

// HandleRegenerateSecret generates a new HMAC secret (admin only).
func HandleRegenerateSecret(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	webhookIdStr := chi.URLParam(r, "webhookId")
	webhookId, err := uuid.Parse(webhookIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid webhook ID"})
		return
	}

	secret, err := business.RegenerateSecret(ctx, webhookId)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"secret": secret})
}

// HandleGetWebhookLogs returns paginated logs for a webhook (admin only).
func HandleGetWebhookLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	webhookIdStr := chi.URLParam(r, "webhookId")
	webhookId, err := uuid.Parse(webhookIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid webhook ID"})
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))

	logs, err := business.GetWebhookLogs(ctx, webhookId, page, pageSize)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": "Failed to retrieve logs"})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"logs": logs})
}

// HandleGetGlobalWebhookLogs returns paginated logs across all webhooks with filters (admin only).
func HandleGetGlobalWebhookLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(*userModel.UserInfo)
	if !ok || userInfo == nil {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"error": "Admin access required"})
		return
	}
	if !userInfo.UserPostgresInfo.IsAdmin {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"error": "Admin access required"})
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	var webhookId *uuid.UUID
	if whID := r.URL.Query().Get("webhook_id"); whID != "" {
		if id, err := uuid.Parse(whID); err == nil {
			webhookId = &id
		}
	}

	var success *bool
	if s := r.URL.Query().Get("success"); s != "" {
		val := s == "true"
		success = &val
	}

	var fromDate, toDate *time.Time
	if f := r.URL.Query().Get("from"); f != "" {
		if t, err := time.Parse(time.RFC3339, f); err == nil {
			fromDate = &t
		}
	}
	if t := r.URL.Query().Get("to"); t != "" {
		if t2, err := time.Parse(time.RFC3339, t); err == nil {
			toDate = &t2
		}
	}

	logs, err := business.GetWebhookLogsGlobal(ctx, webhookId, success, fromDate, toDate, page, pageSize)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": "Failed to retrieve logs"})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"logs": logs})
}

// EventTypes is the single source of truth for webhook event types.
var EventTypes = []string{
	"post.created", "post.updated", "post.deleted",
	"chat.created", "chat.updated", "chat.deleted",
	"task.created", "task.deleted", "task.status_changed", "task.restored",
	"channel.created", "channel.archived",
	"user.joined", "user.left",
}

// HandleGetWebhookEventTypes returns the supported event types (admin only).
func HandleGetWebhookEventTypes(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"event_types": EventTypes})
}

// HandleTestWebhook sends a test event to a webhook (admin only).
func HandleTestWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	webhookIdStr := chi.URLParam(r, "webhookId")
	webhookId, err := uuid.Parse(webhookIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid webhook ID"})
		return
	}

	err = business.TestWebhook(ctx, webhookId)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Test event dispatched"})
}

// HandleIncomingWebhook processes an incoming webhook request (public, token-authenticated).
// Supports signature verification, slash commands, challenge verification, and rich block messages.
// Destination is determined entirely from the payload (Slack-quality flexible routing).
func HandleIncomingWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	token := chi.URLParam(r, "token")
	if token == "" {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"ok": false, "error": "Missing webhook token"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"ok": false, "error": "Failed to read request body"})
		return
	}

	// Challenge verification (Slack-style URL verification)
	var challengePayload struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(body, &challengePayload); err == nil && challengePayload.Challenge != "" {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"challenge": challengePayload.Challenge})
		return
	}

	// Rate limiting: max 30 requests per minute per token (before DB lookup to prevent burning DB)
	if !business.CheckWebhookRateLimit(token) {
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"ok": false, "error": "Rate limit exceeded. Max 30 requests per minute."})
		return
	}

	webhook, err := business.ProcessIncomingWebhook(ctx, token)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/HandleIncomingWebhook Failed to process incoming webhook err: %+v", err)
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"ok": false, "error": "Invalid or inactive webhook"})
		return
	}

	// Signature verification (production security)
	if webhook.SignatureRequired && webhook.Secret != nil && *webhook.Secret != "" {
		sigHeader := r.Header.Get("X-OneCamp-Signature")
		if sigHeader == "" {
			sigHeader = r.Header.Get("X-Slack-Signature")
		}

		if sigHeader == "" {
			helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"ok": false, "error": "Missing signature header"})
			return
		}

		// Try Slack-style signature first (includes timestamp)
		slackTimestamp := r.Header.Get("X-Slack-Request-Timestamp")
		if slackTimestamp != "" && strings.HasPrefix(sigHeader, "v0=") {
			if !business.VerifySlackSignature(*webhook.Secret, body, slackTimestamp, sigHeader) {
				helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"ok": false, "error": "Invalid signature"})
				return
			}
		} else {
			// Try OneCamp v1 signature with timestamp (preferred)
			onecampTimestamp := r.Header.Get("X-OneCamp-Timestamp")
			if onecampTimestamp != "" && strings.HasPrefix(sigHeader, "v1=") {
				if !business.VerifySignatureV1(*webhook.Secret, body, onecampTimestamp, sigHeader) {
					helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"ok": false, "error": "Invalid signature"})
					return
				}
			} else if strings.HasPrefix(sigHeader, "sha256=") {
				// Legacy sha256 signature (no replay protection)
				helpers.LogErrorWithContext(ctx, "controllers/HandleIncomingWebhook Legacy sha256 signature used for webhook %s — please migrate to v1", webhook.Id)
				if !business.VerifySignature(*webhook.Secret, body, sigHeader) {
					helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"ok": false, "error": "Invalid signature"})
					return
				}
			} else {
				helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"ok": false, "error": "Invalid signature format"})
				return
			}
		}
	}

	var payload business.IncomingWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"ok": false, "error": "Invalid JSON payload"})
		return
	}

	// Always use the webhook's configured bot name; ignore payload override to prevent impersonation.
	botName := webhook.BotName

	// Check for slash commands first
	cmd := business.ParseSlashCommand(payload)
	if cmd != nil {
		response, err := business.HandleSlashCommand(ctx, webhook, cmd)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/HandleIncomingWebhook Slash command failed err: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"ok": false, "error": "Command failed"})
			return
		}
		helpers.WriteJSON(w, http.StatusOK, response)
		return
	}

	if payload.Text == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"ok": false, "error": "text field is required"})
		return
	}

	// Build rich text from blocks if provided
	messageText := payload.Text
	if len(payload.Blocks) > 0 {
		richText := business.RenderBlocksToHTML(payload.Blocks)
		if richText != "" {
			messageText = richText
		}
	}

	// -------------------------------------------------------------------------
	// Slack-quality payload-driven routing
	// A single webhook URL can post to ANY channel/DM/group chat.
	// Destination is determined from payload fields, not from webhook.target_type.
	// -------------------------------------------------------------------------

	// Priority 1: explicit channel destination (channel_id or channel field)
	if (payload.ChannelId != nil && *payload.ChannelId != "") || payload.Channel != "" {
		var channelUUID uuid.UUID
		if payload.ChannelId != nil && *payload.ChannelId != "" {
			channelUUID, err = uuid.Parse(*payload.ChannelId)
			if err != nil {
				helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"ok": false, "error": "Invalid channel_id"})
				return
			}
		} else {
			// Support #name lookup
			channelName := strings.TrimPrefix(payload.Channel, "#")
			channelName = strings.TrimSpace(channelName)
			ch, err := channelDomain.GetChannelByName(ctx, channelName)
			if err != nil || ch == nil {
				helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"ok": false, "error": "Channel not found"})
				return
			}
			channelUUID = ch.Id
		}

		postId, err := business.ProcessIncomingMessage(ctx, webhook, channelUUID, messageText, botName)
		if err != nil {
			if err.Error() == "webhook creator is not a member of this channel" {
				helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"ok": false, "error": err.Error()})
				return
			}
			helpers.LogErrorWithContext(ctx, "controllers/HandleIncomingWebhook Post failed err: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"ok": false, "error": "Failed to create message"})
			return
		}
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"ok": true, "msg": "Message posted successfully", "post_id": postId})
		return
	}

	// Priority 2: DM destination
	if payload.DmId != nil && *payload.DmId != "" {
		dmUUID, err := uuid.Parse(*payload.DmId)
		if err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"ok": false, "error": "Invalid dm_id"})
			return
		}
		msgID, err := rootBusiness.ProcessIncomingDM(ctx, webhook, dmUUID, messageText, botName)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/HandleIncomingWebhook DM failed err: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"ok": false, "error": "Failed to create DM message"})
			return
		}
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"ok": true, "msg": "DM message posted", "message_id": msgID})
		return
	}

	// Priority 3: group chat destination
	if payload.GroupChatId != nil && *payload.GroupChatId != "" {
		groupId := strings.TrimSpace(*payload.GroupChatId)
		if groupId == "" {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"ok": false, "error": "Invalid group_chat_id"})
			return
		}
		msgID, err := rootBusiness.ProcessIncomingGroupChat(ctx, webhook, groupId, messageText, botName)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/HandleIncomingWebhook GroupChat failed err: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"ok": false, "error": "Failed to create group chat message"})
			return
		}
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"ok": true, "msg": "Group chat message posted", "message_id": msgID})
		return
	}

	// Priority 4: fallback to webhook's default channel_id
	if webhook.ChannelId != nil {
		postId, err := business.ProcessIncomingMessage(ctx, webhook, *webhook.ChannelId, messageText, botName)
		if err != nil {
			if err.Error() == "webhook creator is not a member of this channel" {
				helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"ok": false, "error": err.Error()})
				return
			}
			helpers.LogErrorWithContext(ctx, "controllers/HandleIncomingWebhook Post failed err: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"ok": false, "error": "Failed to create message"})
			return
		}
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"ok": true, "msg": "Message posted successfully", "post_id": postId})
		return
	}

	// No destination found
	helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"ok": false, "error": "No destination specified. Provide channel_id, channel, dm_id, group_chat_id, or set a default channel on the webhook."})
}

// isWebhookValidationError determines if a webhook business error is a client validation error.
func isWebhookValidationError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	validationPrefixes := []string{
		"invalid webhook type",
		"outgoing webhook requires",
		"invalid target_url",
		"failed to marshal",
		"webhook not found",
	}
	for _, prefix := range validationPrefixes {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}
