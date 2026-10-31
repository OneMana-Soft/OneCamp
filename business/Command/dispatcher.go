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
	cardsBusiness "github.com/akashc777/OneCamp/business/Cards"
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
// A command stands where commandPlace puts it: an app that isn't set up is
// offered to admins to set up and kept from members, whose "/giphy" only said
// an admin had to add a key; one turned off or removed is offered to nobody,
// and so is a command this build can't run (wired).
func GetCatalog(ctx context.Context, user userModels.UserInfo) (*commandAdapter.CatalogResponse, error) {
	userUUID := user.UserDgraphInfo.Uuid
	// An admin's API token acts as a member, so it reads a member's menu.
	admin := user.UserPostgresInfo.IsAdmin && !helpers.ActsThroughToken(ctx)
	version := catalogVersion(ctx)
	cacheArg := userUUID + ":" + version + ":" + strconv.FormatBool(admin)

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
	installed, err := slashModel.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	removed, err := slashModel.RemovedAppSlugs(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetCatalog removed apps err: %+v", err)
		return nil, err
	}
	apps := make(map[string]*latestApp, len(installed)+len(removed))
	for slug := range removed {
		apps[slug] = &latestApp{Removed: true}
	}
	for _, a := range installed {
		apps[a.Slug] = &latestApp{App: a}
	}
	bags := map[uuid.UUID]*appSecretBag{}

	resp := &commandAdapter.CatalogResponse{Commands: make([]commandAdapter.CatalogCommand, 0, len(cmds))}
	for _, c := range cmds {
		if !wired(c) {
			continue
		}
		t, ok := commandTemplate(c)
		var app *latestApp
		var bag *appSecretBag
		if ok {
			app = apps[t.Slug]
		}
		if app != nil && !app.Removed && app.IsEnabled {
			if _, read := bags[app.Id]; !read {
				b, err := loadAppSecrets(ctx, app.Id)
				if err != nil {
					// Secrets that can't be read serve no better than none: the
					// app is offered to set up again.
					helpers.LogErrorWithContext(ctx, "business/GetCatalog secrets of %s err: %+v", app.Slug, err)
				}
				bags[app.Id] = b
			}
			bag = bags[app.Id]
		}
		place := commandPlace(t, ok, app, bag)
		if place == menuHidden || (place == menuSetup && !admin) {
			continue
		}
		entry := commandAdapter.CatalogCommand{
			Command:     c.Command,
			Description: c.Description,
			IsBuiltin:   c.IsBuiltin,
		}
		if place == menuSetup {
			entry.Setup = &commandAdapter.CatalogSetup{Slug: t.Slug, Name: t.Name}
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
		if _, interactive := interactionRegistry[name]; interactive {
			sealAnswer(resp, cardsBusiness.Owner{Kind: cardsBusiness.OwnerBuiltin, ID: name}, user.UserDgraphInfo.Uuid)
		}
		return resp, nil

	case slashModel.ExecExternal:
		return dispatchExternal(ctx, cmd, cc)

	default:
		return errorResponse("Command `/%s` has an unknown execution mode.", name), nil
	}
}

// HandleInteract is POST /command/interact, kept so a tab opened before the
// cards change gets an answer that names the way forward rather than a 404.
// Every card is pressed through POST /cards/act now, which checks the press
// against the card the server sealed (business/Cards): this route ran
// whatever press a client described. A card from before has no seal, so it
// asks for a fresh one.
func HandleInteract(command string) *commandAdapter.CommandResponse {
	return errorResponse("This card is out of date. Run `/%s` again for a fresh one.", normalizeCommand(command))
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
		Mentions:  req.Mentions,
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
		return errorResponse("`/%s` isn't set up yet. An admin can finish setting it up in Admin › Apps.", cc.Command), nil
	}

	signingSecret := resolveSigningSecret(ctx, cmd)

	// Fire-and-forget: the app replies async; we ACK immediately so the
	// composer feels instant.
	helpers.GoSafeNamed("forwardToApp", func() { forwardToApp(appCtx, handlerURL, signingSecret, cmd, cc) })

	return &commandAdapter.CommandResponse{
		ResponseType: "ephemeral",
		Text:         fmt.Sprintf("Working on `/%s`…", cc.Command),
		Ephemeral:    true,
		TriggerID:    cc.TriggerID,
	}, nil
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
		if app, err := slashModel.GetAppByID(ctx, *cmd.AppId); err == nil && app != nil {
			return appSigningSecret(app)
		}
	}
	return ""
}

// appSigningSecret is the secret an app checks our signature with: stored
// encrypted (helpers.EncryptSecret), so it is decrypted here. Commands were
// signed with the stored ciphertext itself, which no app could check; only
// the Test probe decrypted it. A value that doesn't decrypt is one stored
// before secrets were encrypted, and is the secret as it stands.
func appSigningSecret(app *slashModel.App) string {
	if app == nil || app.SigningSecret == nil || *app.SigningSecret == "" {
		return ""
	}
	if plain, err := helpers.DecryptSecret(*app.SigningSecret); err == nil {
		return plain
	}
	return *app.SigningSecret
}

// forwardToApp posts the command payload to the app handler, validating the
// URL against SSRF and signing the body. The app's response (if any) is
// delivered back to the invoker over MQTT.
func forwardToApp(ctx context.Context, handlerURL, signingSecret string, cmd *slashModel.SlashCommand, cc CommandContext) {
	if err := forwardCheck(handlerURL); err != nil {
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
	// Where the app may answer later: up to five times in 30 minutes.
	if cmd.AppId != nil {
		if u := newResponseURL(ctx, *cmd.AppId, deref(cmd.AppName), deref(cmd.AppIconUrl), cc); u != "" {
			payload["response_url"] = u
		}
	}

	resp := postSigned(ctx, handlerURL, signingSecret, payload)
	if resp != nil {
		if resp.TriggerID == "" {
			resp.TriggerID = cc.TriggerID
		}
		if cmd.AppId != nil {
			sealAnswer(resp, appOwner(cmd.AppId.String()), cc.User.UserDgraphInfo.Uuid)
		}
		deliverAsync(ctx, cc, resp)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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

	client := forwardClient()
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

// forwardCheck and forwardClient are how a command, its answer's card and a
// press reach an app, Giphy's API included (giphyQuery): an HTTPS address on
// a public network, through the SSRF-safe client. Seams for the tests that
// serve an app on loopback (forwardToThisNetworkForTest_integration.go, and
// giphyPicker_test.go's Giphy stand-in).
var (
	forwardCheck  = func(raw string) error { _, err := helpers.ValidateOutboundURL(raw, false); return err }
	forwardClient = func() *http.Client { return helpers.SSRFSafeClient(false) }
)

// signV1 computes the v1 HMAC-SHA256 signature over "v1:timestamp:body",
// matching the existing webhook verification scheme.
func signV1(secret string, body []byte, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v1:" + timestamp + ":"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
