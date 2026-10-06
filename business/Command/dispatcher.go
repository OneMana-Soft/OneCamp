package business

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	"github.com/akashc777/OneCamp/helpers"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

const (
	// commandRateLimit caps executions per user per minute.
	commandRateLimit = 60
	// externalTimeout bounds outbound app calls.
	externalTimeout = 10 * time.Second
)

// GetCatalog returns the scope-filtered command list for a user, cached in
// Redis so the composer typeahead never hits Postgres on the hot path. The
// cache key embeds a workspace-wide catalog version so an admin
// install/toggle/remove invalidates every user's cache at once (bumpCatalogVersion).
func GetCatalog(ctx context.Context, user userModels.UserInfo) (*commandAdapter.CatalogResponse, error) {
	userUUID := user.UserDgraphInfo.Uuid
	version := catalogVersion(ctx)
	cacheArg := userUUID + ":" + version

	// Cache hit.
	var cached commandAdapter.CatalogResponse
	if found, _ := redisStore.GetJSON(ctx, registry.CommandCatalog, []string{cacheArg}, &cached); found {
		return &cached, nil
	}

	teamIDs := teamScopeIDs(user)
	channelIDs := channelScopeIDs(user)

	cmds, err := slashModel.ListCommandsForScopes(ctx, teamIDs, channelIDs)
	if err != nil {
		return nil, err
	}

	resp := &commandAdapter.CatalogResponse{Commands: make([]commandAdapter.CatalogCommand, 0, len(cmds))}
	for _, c := range cmds {
		entry := commandAdapter.CatalogCommand{
			Command:     c.Command,
			Description: c.Description,
			IsBuiltin:   c.IsBuiltin,
		}
		if c.UsageHint != nil {
			entry.UsageHint = *c.UsageHint
		}
		if c.AppSlug != nil {
			entry.AppSlug = *c.AppSlug
		}
		if c.AppName != nil {
			entry.AppName = *c.AppName
		}
		if c.AppIconUrl != nil {
			entry.IconURL = *c.AppIconUrl
		}
		resp.Commands = append(resp.Commands, entry)
	}

	_ = redisStore.SetJSON(ctx, registry.CommandCatalog, []string{cacheArg}, resp)
	return resp, nil
}

// catalogVersion returns the current workspace-wide catalog version token. It
// is embedded in every user's catalog cache key so a single bump (on admin
// app install/toggle/remove) invalidates all cached catalogs at once without
// SCAN-deleting per-user keys. Defaults to "0" when Redis is unavailable or
// unset (so dev without Redis still works — it just always recomputes).
func catalogVersion(ctx context.Context) string {
	v, found, _ := redisStore.GetString(ctx, registry.CommandCatalogVersion, nil)
	if !found || v == "" {
		return "0"
	}
	return v
}

// bumpCatalogVersion advances the catalog version so every cached catalog is
// invalidated. Called after any admin change to the command/app registry.
func bumpCatalogVersion(ctx context.Context) {
	// A monotonically-increasing token is enough; nanosecond timestamp is
	// simple, collision-free for this purpose, and human-readable in ops.
	token := fmt.Sprintf("%d", time.Now().UnixNano())
	_ = redisStore.SetString(ctx, registry.CommandCatalogVersion, nil, token)
}

// Execute is the entry point for POST /command/execute. It resolves the
// command within the user's scopes, enforces the rate limit, and routes to the
// correct execution mode.
func Execute(ctx context.Context, user userModels.UserInfo, req commandAdapter.ExecuteCommandRequest) (*commandAdapter.CommandResponse, error) {
	name := normalizeCommand(req.Command)
	if name == "" {
		return errorResponse("No command provided."), nil
	}

	// Rate limit per user.
	if !checkRate(ctx, user.UserDgraphInfo.Uuid) {
		return errorResponse("You're sending commands too fast. Try again in a moment."), nil
	}

	teamIDs := teamScopeIDs(user)
	channelIDs := channelScopeIDs(user)

	cmd, err := slashModel.ResolveCommand(ctx, name, teamIDs, channelIDs)
	if err != nil {
		logErr(ctx, "Execute/ResolveCommand", err)
		return errorResponse("Couldn't look up that command."), nil
	}
	if cmd == nil {
		return suggestUnknown(ctx, user, name), nil
	}

	cc := buildContext(user, name, req)

	switch cmd.ExecMode {
	case slashModel.ExecInline, slashModel.ExecDeferred, slashModel.ExecInteractive:
		handler, ok := commandRegistry[name]
		if !ok {
			// Registry/DB drift: catalog says built-in but no handler wired.
			return errorResponse("Command `/%s` is registered but unavailable.", name), nil
		}
		resp, hErr := handler(ctx, cc)
		if hErr != nil {
			logErr(ctx, "Execute/handler:"+name, hErr)
			return errorResponse("`/%s` failed: %v", name, hErr), nil
		}
		if resp != nil && resp.TriggerID == "" {
			resp.TriggerID = req.TriggerID
		}
		return resp, nil

	case slashModel.ExecExternal:
		return dispatchExternal(ctx, cmd, cc)

	default:
		return errorResponse("Command `/%s` has an unknown execution mode.", name), nil
	}
}

// HandleInteract processes a button/select click on an interactive card.
func HandleInteract(ctx context.Context, user userModels.UserInfo, ir commandAdapter.InteractRequest) (*commandAdapter.CommandResponse, error) {
	name := normalizeCommand(ir.Command)
	if !checkRate(ctx, user.UserDgraphInfo.Uuid) {
		return errorResponse("Too many actions. Slow down a moment."), nil
	}

	handler, ok := interactionRegistry[name]
	if !ok {
		// Could be an external app — forward the interaction.
		teamIDs := teamScopeIDs(user)
		channelIDs := channelScopeIDs(user)
		cmd, err := slashModel.ResolveCommand(ctx, name, teamIDs, channelIDs)
		if err == nil && cmd != nil && cmd.ExecMode == slashModel.ExecExternal {
			cc := CommandContext{User: user, Command: name, TriggerID: ir.TriggerID}
			if ir.ChannelID != nil {
				if id, perr := uuid.Parse(*ir.ChannelID); perr == nil {
					cc.ChannelID = &id
				}
			}
			return dispatchExternalInteraction(ctx, cmd, cc, ir)
		}
		return errorResponse("This action is no longer available."), nil
	}

	cc := CommandContext{User: user, Command: name, TriggerID: ir.TriggerID, DmGroupID: ir.DmGroupID}
	if ir.ChannelID != nil {
		if id, perr := uuid.Parse(*ir.ChannelID); perr == nil {
			cc.ChannelID = &id
		}
	}
	resp, err := handler(ctx, cc, ir)
	if err != nil {
		logErr(ctx, "HandleInteract/"+name, err)
		return errorResponse("That action failed: %v", err), nil
	}
	if resp != nil && resp.TriggerID == "" {
		resp.TriggerID = ir.TriggerID
	}
	return resp, nil
}

// buildContext assembles a CommandContext from the request.
func buildContext(user userModels.UserInfo, name string, req commandAdapter.ExecuteCommandRequest) CommandContext {
	cc := CommandContext{
		User:      user,
		Command:   name,
		Text:      trimSpace(req.Text),
		DmGroupID: req.DmGroupID,
		ThreadTs:  req.ThreadTs,
		Timezone:  req.Timezone,
		TriggerID: req.TriggerID,
	}
	if req.ChannelID != nil && *req.ChannelID != "" {
		if id, err := uuid.Parse(*req.ChannelID); err == nil {
			cc.ChannelID = &id
		}
	}
	return cc
}

// suggestUnknown returns a friendly "did you mean" using the catalog.
func suggestUnknown(ctx context.Context, user userModels.UserInfo, name string) *commandAdapter.CommandResponse {
	catalog, err := GetCatalog(ctx, user)
	if err != nil || catalog == nil {
		return errorResponse("Unknown command `/%s`. Type `/` to see what's available.", name)
	}
	var best string
	for _, c := range catalog.Commands {
		if len(c.Command) > 0 && len(name) > 0 && c.Command[0] == name[0] {
			best = c.Command
			break
		}
	}
	if best != "" {
		return errorResponse("Unknown command `/%s`. Did you mean `/%s`?", name, best)
	}
	return errorResponse("Unknown command `/%s`. Type `/` to see what's available.", name)
}

// checkRate enforces a fixed-window per-user command rate limit.
func checkRate(ctx context.Context, userUUID string) bool {
	res := redisStore.AllowFixedWindow(ctx, registry.CommandRate, []string{userUUID}, commandRateLimit)
	return res.Allowed
}

// --- External app dispatch (Slack-compatible) ---

// dispatchExternal forwards a command to an installed app's handler URL. The
// outbound call is SSRF-guarded and HMAC-signed; the immediate response is an
// ephemeral acknowledgement (Slack pattern), and the app's real reply arrives
// asynchronously via the response callback delivered over MQTT.
func dispatchExternal(ctx context.Context, cmd *slashModel.SlashCommand, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	handlerURL := resolveHandlerURL(ctx, cmd)
	if handlerURL == "" {
		return errorResponse("`/%s` has no handler configured.", cc.Command), nil
	}

	signingSecret := resolveSigningSecret(ctx, cmd)

	// Fire-and-forget: the app replies async; we ACK immediately so the
	// composer feels instant.
	go forwardToApp(appCtx, handlerURL, signingSecret, cmd, cc)

	return &commandAdapter.CommandResponse{
		ResponseType: "ephemeral",
		Text:         fmt.Sprintf("Working on `/%s`…", cc.Command),
		Ephemeral:    true,
		TriggerID:    cc.TriggerID,
	}, nil
}

func dispatchExternalInteraction(ctx context.Context, cmd *slashModel.SlashCommand, cc CommandContext, ir commandAdapter.InteractRequest) (*commandAdapter.CommandResponse, error) {
	handlerURL := resolveHandlerURL(ctx, cmd)
	if handlerURL == "" {
		return errorResponse("This action is no longer available."), nil
	}
	signingSecret := resolveSigningSecret(ctx, cmd)
	go forwardInteractionToApp(appCtx, handlerURL, signingSecret, cmd, cc, ir)
	return &commandAdapter.CommandResponse{ResponseType: "ephemeral", Ephemeral: true, TriggerID: cc.TriggerID}, nil
}

func resolveHandlerURL(ctx context.Context, cmd *slashModel.SlashCommand) string {
	if cmd.HandlerUrl != nil && *cmd.HandlerUrl != "" {
		return *cmd.HandlerUrl
	}
	if cmd.AppId != nil {
		if app, err := slashModel.GetAppByID(ctx, *cmd.AppId); err == nil && app != nil && app.HandlerUrl != nil {
			return *app.HandlerUrl
		}
	}
	return ""
}

func resolveSigningSecret(ctx context.Context, cmd *slashModel.SlashCommand) string {
	if cmd.AppId != nil {
		if app, err := slashModel.GetAppByID(ctx, *cmd.AppId); err == nil && app != nil && app.SigningSecret != nil {
			return *app.SigningSecret
		}
	}
	return ""
}

// forwardToApp posts the command payload to the app handler, validating the
// URL against SSRF and signing the body. The app's response (if any) is
// delivered back to the invoker over MQTT.
func forwardToApp(ctx context.Context, handlerURL, signingSecret string, cmd *slashModel.SlashCommand, cc CommandContext) {
	if _, err := helpers.ValidateOutboundURL(handlerURL, false); err != nil {
		logErr(ctx, "forwardToApp/SSRF", err)
		deliverAsync(ctx, cc, errorResponse("`/%s` could not reach its app.", cc.Command))
		return
	}

	payload := map[string]interface{}{
		"command":    "/" + cc.Command,
		"text":       cc.Text,
		"user_id":    cc.User.UserDgraphInfo.Uuid,
		"user_name":  cc.User.UserDgraphInfo.UserName,
		"trigger_id": cc.TriggerID,
		"timezone":   cc.Timezone,
	}
	if cc.ChannelID != nil {
		payload["channel_id"] = cc.ChannelID.String()
	}
	if cc.DmGroupID != nil {
		payload["dm_group_id"] = *cc.DmGroupID
	}

	resp := postSigned(ctx, handlerURL, signingSecret, payload)
	if resp != nil {
		deliverAsync(ctx, cc, resp)
	}
}

func forwardInteractionToApp(ctx context.Context, handlerURL, signingSecret string, cmd *slashModel.SlashCommand, cc CommandContext, ir commandAdapter.InteractRequest) {
	if _, err := helpers.ValidateOutboundURL(handlerURL, false); err != nil {
		logErr(ctx, "forwardInteractionToApp/SSRF", err)
		return
	}
	payload := map[string]interface{}{
		"type":       "block_action",
		"command":    "/" + cc.Command,
		"action_id":  ir.ActionID,
		"value":      ir.Value,
		"state":      ir.State,
		"user_id":    cc.User.UserDgraphInfo.Uuid,
		"trigger_id": cc.TriggerID,
	}
	resp := postSigned(ctx, handlerURL, signingSecret, payload)
	if resp != nil {
		resp.ReplaceOriginal = true
		deliverAsync(ctx, cc, resp)
	}
}

// doSignedProbe sends a signed, side-effect-free connectivity probe and returns
// the raw HTTP response (status only matters to the caller). Used by the admin
// "Test" action for external apps. SSRF is validated by the caller.
func doSignedProbe(ctx context.Context, url, secret string, payload map[string]interface{}) *http.Response {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "OneCamp-Commands/1.0")
	if secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		req.Header.Set("X-OneCamp-Timestamp", ts)
		req.Header.Set("X-OneCamp-Signature", "v1="+signV1(secret, body, ts))
	}
	client := helpers.SSRFSafeClient(false)
	client.Timeout = 8 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	// Drain + close so the connection can be reused; caller only needs status.
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp
}

// postSigned marshals the payload, signs it (v1 HMAC), POSTs via the
// SSRF-safe client, and parses a CommandResponse from the reply.
func postSigned(ctx context.Context, url, secret string, payload map[string]interface{}) *commandAdapter.CommandResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		logErr(ctx, "postSigned/marshal", err)
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		logErr(ctx, "postSigned/newrequest", err)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "OneCamp-Commands/1.0")
	if secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		req.Header.Set("X-OneCamp-Timestamp", ts)
		req.Header.Set("X-OneCamp-Signature", "v1="+signV1(secret, body, ts))
	}

	client := helpers.SSRFSafeClient(false)
	client.Timeout = externalTimeout
	httpResp, err := client.Do(req)
	if err != nil {
		logErr(ctx, "postSigned/do", err)
		return nil
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil
	}

	raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if len(raw) == 0 {
		return nil
	}
	var resp commandAdapter.CommandResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		logErr(ctx, "postSigned/unmarshal", err)
		return nil
	}
	if resp.ResponseType == "" {
		resp.ResponseType = "ephemeral"
		resp.Ephemeral = true
	}
	return &resp
}

// signV1 computes the v1 HMAC-SHA256 signature over "v1:timestamp:body",
// matching the existing webhook verification scheme.
func signV1(secret string, body []byte, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v1:" + timestamp + ":"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// InvalidateCatalog drops a user's cached catalog. The admin install/toggle
// path can't enumerate every user cheaply, so callers may also rely on the
// short 5-minute TTL for eventual consistency; this is the targeted bust.
func InvalidateCatalog(ctx context.Context, userUUID string) {
	version := catalogVersion(ctx)
	_ = redisStore.Delete(ctx, registry.CommandCatalog, []string{userUUID + ":" + version})
}
