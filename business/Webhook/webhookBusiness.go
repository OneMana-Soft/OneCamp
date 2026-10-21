package business

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	botpost "github.com/akashc777/OneCamp/business/BotPost"
	hooksBusiness "github.com/akashc777/OneCamp/business/Hooks"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	domain "github.com/akashc777/OneCamp/domain/Webhook"
	"github.com/akashc777/OneCamp/helpers"
	webhookModel "github.com/akashc777/OneCamp/models/postgres/Webhook"
	logModel "github.com/akashc777/OneCamp/models/postgres/WebhookLog"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

const (
	DefaultLogRetentionDays = 30
	OutgoingTimeout         = 10 * time.Second
	MaxResponseBodyLog      = 2048
	WebhookRateLimitPerMin  = 30
	SignatureVersionV1      = hooksBusiness.SignatureVersion
	SignatureTolerance      = 5 * time.Minute
)

var (
	// triggerWordRegexCache maps webhook ID -> compiled regexes for trigger words.
	triggerWordRegexCache sync.Map

	// appCtx is the application-level context used by background goroutines.
	// It is set at startup via SetAppContext.
	appCtx = context.Background()
)

type rateLimitEntry struct {
	count       int
	windowStart time.Time
}

// CheckWebhookRateLimit returns true if the webhook is within its rate limit.
// Uses the registry-backed sliding-window limiter when Redis is available;
// falls back to an in-memory map when Redis is down so a hard outage of
// Redis still rate-limits incoming traffic per replica.
func CheckWebhookRateLimit(webhookID string) bool {
	if !redisStore.IsAvailable() {
		return checkWebhookRateLimitInMemory(webhookID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res := redisStore.AllowSlidingWindow(ctx, registry.WebhookRateLimit, []string{webhookID}, WebhookRateLimitPerMin, 60)
	if res.Unchecked {
		// The call failed: an outage after boot, when the client exists but
		// Redis doesn't answer. Counted here, as with no client, rather than
		// let through.
		return checkWebhookRateLimitInMemory(webhookID)
	}
	if !res.Allowed {
		webhookRateLimitedTotal.Inc()
		return false
	}
	return true
}

func checkWebhookRateLimitInMemory(webhookID string) bool {
	now := time.Now()
	webhookRateLimiter.mu.Lock()
	defer webhookRateLimiter.mu.Unlock()

	entry, exists := webhookRateLimiter.counters[webhookID]
	if !exists || now.Sub(entry.windowStart) > time.Minute {
		webhookRateLimiter.counters[webhookID] = &rateLimitEntry{count: 1, windowStart: now}
		return true
	}

	if entry.count >= WebhookRateLimitPerMin {
		return false
	}

	entry.count++
	return true
}

// webhookRateLimiter is the fallback in-process rate limiter.
var webhookRateLimiter = struct {
	mu       sync.RWMutex
	counters map[string]*rateLimitEntry
}{counters: make(map[string]*rateLimitEntry)}

// IncomingWebhookPayload is the expected format for incoming webhook messages.
// Supports simple text and Slack-style blocks, which business/Cards reads
// (Reduce) and draws as the message's body (HTML).
// Destination is determined entirely from the payload (Slack-quality flexible routing):
//   - channel_id or channel → post to channel
//   - dm_id → direct message
//   - group_chat_id → group chat message
//   - none of the above → fallback to webhook's default channel_id
//
// The webhook's target_type is no longer used for routing.
type IncomingWebhookPayload struct {
	Text        string          `json:"text"`
	ChannelId   *string         `json:"channel_id,omitempty"`
	Channel     string          `json:"channel,omitempty"` // Slack-style alias for channel_id (UUID or #name)
	DmId        *string         `json:"dm_id,omitempty"`
	GroupChatId *string         `json:"group_chat_id,omitempty"`
	BotName     *string         `json:"bot_name,omitempty"`
	Blocks      json.RawMessage `json:"blocks,omitempty"`
	Ephemeral   *bool           `json:"ephemeral,omitempty"` // If true, only visible to target user
	ThreadTs    *string         `json:"thread_ts,omitempty"` // For threading
	Markdown    *bool           `json:"markdown,omitempty"`  // Parse text as markdown
	Attachments []Attachment    `json:"attachments,omitempty"`

	// Command is read only to say where slash commands went: an incoming
	// webhook posts messages, and a slash command is an app's (Admin › Apps).
	// It used to run the webhook's own registry (/help, /status), which never
	// answered anything but "Unknown command" (see SlashCommandsMoved).
	Command string `json:"command,omitempty"`
}

// SlashCommandsMoved is the answer to a payload that names a command.
const SlashCommandsMoved = "Incoming webhooks only post messages. Slash commands are made by apps: add one in Admin › Apps."

type Attachment struct {
	Color     string     `json:"color,omitempty"`
	Title     string     `json:"title,omitempty"`
	TitleLink string     `json:"title_link,omitempty"`
	Text      string     `json:"text,omitempty"`
	Fields    []AttField `json:"fields,omitempty"`
	Footer    string     `json:"footer,omitempty"`
	Ts        int64      `json:"ts,omitempty"`
}

type AttField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// OutgoingWebhookPayload is the format sent to external services.
type OutgoingWebhookPayload struct {
	Event     string      `json:"event"`
	Timestamp string      `json:"timestamp"`
	Data      interface{} `json:"data"`
	// EventID is the event's id: the same on every try and every replay
	// (business/Hooks), as X-OneCamp-Delivery says it, so a receiver can tell
	// a retry from a new event.
	EventID string `json:"event_id,omitempty"`
}

// WebhookCreateInput captures creation parameters.
type WebhookCreateInput struct {
	Name         string     `json:"name"`
	Description  *string    `json:"description,omitempty"`
	Type         string     `json:"type"`
	TargetUrl    *string    `json:"target_url,omitempty"`
	ChannelId    *uuid.UUID `json:"channel_id,omitempty"`
	Events       []string   `json:"events,omitempty"`
	BotName      string     `json:"bot_name"`
	BotAvatarUrl *string    `json:"bot_avatar_url,omitempty"`
	// ScopeType and ScopeEntityId say what an outgoing webhook is sent events
	// from: "org" (the whole workspace), "channel" or "project", with the
	// channel's or project's id (outgoingPrivacy.go).
	ScopeType     string     `json:"scope_type,omitempty"`
	ScopeEntityId *uuid.UUID `json:"scope_entity_id,omitempty"`
}

// sanitizeEvents removes empty strings and duplicates from an event slice.
func sanitizeEvents(events []string) []string {
	if len(events) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(events))
	out := make([]string, 0, len(events))
	for _, ev := range events {
		if ev == "" || seen[ev] {
			continue
		}
		seen[ev] = true
		out = append(out, ev)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// WebhookUpdateInput captures update parameters.
type WebhookUpdateInput struct {
	Name         string     `json:"name"`
	Description  *string    `json:"description,omitempty"`
	TargetUrl    *string    `json:"target_url,omitempty"`
	ChannelId    *uuid.UUID `json:"channel_id,omitempty"`
	Events       []string   `json:"events,omitempty"`
	IsActive     bool       `json:"is_active"`
	BotName      string     `json:"bot_name"`
	BotAvatarUrl *string    `json:"bot_avatar_url,omitempty"`
	// ScopeType and ScopeEntityId as WebhookCreateInput's; an update that
	// names no scope keeps the one it had.
	ScopeType     string     `json:"scope_type,omitempty"`
	ScopeEntityId *uuid.UUID `json:"scope_entity_id,omitempty"`
}

// defaultBotName labels an incoming webhook's messages when it names no bot.
const defaultBotName = "Webhook Bot"

// botNameOf is the name a webhook's messages are labelled with: name, trimmed,
// or fallback when that leaves nothing. Every message a webhook posts carries
// one: a bot message with no label is read as a model's Markdown
// (business/BotPost badgeHTML), and a webhook's text is whatever its URL's
// holder sends. Pure.
func botNameOf(name, fallback string) string {
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	return fallback
}

// generateSecureToken creates a cryptographically secure random token.
func generateSecureToken(length int) (string, error) {
	b := make([]byte, length)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b), nil
}

// signPayload creates an HMAC-SHA256 signature.
func signPayload(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// CreateWebhook creates a new webhook with auto-generated token and optional secret.
func CreateWebhook(ctx context.Context, input WebhookCreateInput, createdBy uuid.UUID) (*webhookModel.Webhook, error) {
	// Validate type
	if input.Type != "incoming" && input.Type != "outgoing" {
		return nil, fmt.Errorf("invalid webhook type: %s", input.Type)
	}

	// Validate outgoing webhook has target_url
	if input.Type == "outgoing" && (input.TargetUrl == nil || *input.TargetUrl == "") {
		return nil, fmt.Errorf("outgoing webhook requires a target_url")
	}

	// Validate outgoing webhook target_url is safe
	if input.Type == "outgoing" && input.TargetUrl != nil && *input.TargetUrl != "" {
		if _, err := helpers.ValidateOutboundURL(*input.TargetUrl, false); err != nil {
			return nil, fmt.Errorf("invalid target_url: %w", err)
		}
	}

	// Validate incoming webhook has channel_id or will accept it in payload
	// (channel_id is optional for incoming — can be specified in payload)

	// What it is sent events from, checked before anything is written.
	scope, scopeEntity, err := webhookScope(ctx, input.Type, input.ScopeType, input.ScopeEntityId)
	if err != nil {
		return nil, err
	}

	// Generate secure token (32 bytes = 43 chars base64url)
	token, err := generateSecureToken(32)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateWebhook Failed to generate token err: %+v", err)
		return nil, fmt.Errorf("failed to generate secure token")
	}

	// Generate HMAC secret for outgoing webhooks
	var secret *string
	if input.Type == "outgoing" {
		s, err := generateSecureToken(32)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"business/CreateWebhook Failed to generate secret err: %+v", err)
			return nil, fmt.Errorf("failed to generate signing secret")
		}
		secret = &s
	}

	// Sanitize and marshal events
	input.Events = sanitizeEvents(input.Events)
	var eventsJson *string
	if len(input.Events) > 0 {
		evBytes, err := json.Marshal(input.Events)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal events")
		}
		evStr := string(evBytes)
		eventsJson = &evStr
	}

	id := uuid.New()
	now := time.Now()

	input.BotName = botNameOf(input.BotName, defaultBotName)

	err = domain.CreateWebhook(ctx, id, input.Name, input.Description, input.Type, token, secret, input.TargetUrl, input.ChannelId, eventsJson, createdBy, input.BotName, input.BotAvatarUrl, nil, now)
	if err != nil {
		return nil, err
	}
	if scope != ScopeOrg {
		if err := domain.SetWebhookScope(ctx, id, scope, scopeEntity, now); err != nil {
			return nil, err
		}
	}

	// Invalidate the dispatch cache so the next event fan-out picks up the
	// new webhook without waiting for the TTL to expire.
	bumpDispatchCacheVersion()

	// Default signature_required to true for new webhooks
	if input.Type == "incoming" {
		_ = domain.UpdateWebhookSignatureRequired(ctx, id, true, now)
	}

	webhook, err := domain.GetWebhookById(ctx, id)
	if err != nil {
		return nil, err
	}

	return webhook, nil
}

// UpdateWebhook updates an existing webhook's configuration.
func UpdateWebhook(ctx context.Context, webhookId uuid.UUID, input WebhookUpdateInput) (*webhookModel.Webhook, error) {
	existing, err := domain.GetWebhookById(ctx, webhookId)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("webhook not found")
	}

	// Validate outgoing webhook has target_url
	if existing.Type == "outgoing" && (input.TargetUrl == nil || *input.TargetUrl == "") {
		return nil, fmt.Errorf("outgoing webhook requires a target_url")
	}

	// Validate outgoing webhook target_url is safe
	if existing.Type == "outgoing" && input.TargetUrl != nil && *input.TargetUrl != "" {
		if _, err := helpers.ValidateOutboundURL(*input.TargetUrl, false); err != nil {
			return nil, fmt.Errorf("invalid target_url: %w", err)
		}
	}

	// What it is sent events from, when the update says.
	var scope string
	var scopeEntity *uuid.UUID
	if input.ScopeType != "" {
		if scope, scopeEntity, err = webhookScope(ctx, existing.Type, input.ScopeType, input.ScopeEntityId); err != nil {
			return nil, err
		}
	}

	input.Events = sanitizeEvents(input.Events)
	var eventsJson *string
	if len(input.Events) > 0 {
		evBytes, err := json.Marshal(input.Events)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal events")
		}
		evStr := string(evBytes)
		eventsJson = &evStr
	}

	input.BotName = botNameOf(input.BotName, botNameOf(existing.BotName, defaultBotName))

	now := time.Now()

	// Reset failure_count when webhook is re-enabled
	if !existing.IsActive && input.IsActive {
		_ = domain.UpdateWebhookTriggerInfo(ctx, webhookId, now, 0)
	}

	err = domain.UpdateWebhook(ctx, webhookId, input.Name, input.Description, input.TargetUrl, input.ChannelId, eventsJson, input.IsActive, input.BotName, input.BotAvatarUrl, existing.Metadata, now)
	if err != nil {
		return nil, err
	}
	if scope != "" {
		if err := domain.SetWebhookScope(ctx, webhookId, scope, scopeEntity, now); err != nil {
			return nil, err
		}
	}

	// Any of name / target / events / scope / active flag could change
	// the dispatch result. Cheaper to bump unconditionally than to
	// diff the input.
	bumpDispatchCacheVersion()

	return domain.GetWebhookById(ctx, webhookId)
}

// DeleteWebhook soft-deletes a webhook.
func DeleteWebhook(ctx context.Context, webhookId uuid.UUID) error {
	existing, err := domain.GetWebhookById(ctx, webhookId)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("webhook not found")
	}

	now := time.Now()
	if err := domain.SoftDeleteWebhook(ctx, webhookId, now); err != nil {
		return err
	}
	bumpDispatchCacheVersion()
	evictTriggerRegexCache(webhookId)
	return nil
}

// RegenerateToken generates a new token for the webhook.
func RegenerateToken(ctx context.Context, webhookId uuid.UUID) (string, error) {
	token, err := generateSecureToken(32)
	if err != nil {
		return "", fmt.Errorf("failed to generate token")
	}

	now := time.Now()
	err = domain.RegenerateWebhookToken(ctx, webhookId, token, now)
	if err != nil {
		return "", err
	}

	// Reset failure count on token regeneration
	_ = domain.UpdateWebhookTriggerInfo(ctx, webhookId, now, 0)

	// Cached webhook list embeds the token, so invalidate.
	bumpDispatchCacheVersion()

	return token, nil
}

// RegenerateSecret generates a new HMAC secret.
func RegenerateSecret(ctx context.Context, webhookId uuid.UUID) (string, error) {
	secret, err := generateSecureToken(32)
	if err != nil {
		return "", fmt.Errorf("failed to generate secret")
	}

	now := time.Now()
	err = domain.RegenerateWebhookSecret(ctx, webhookId, secret, now)
	if err != nil {
		return "", err
	}

	// Reset failure count on secret regeneration
	_ = domain.UpdateWebhookTriggerInfo(ctx, webhookId, now, 0)

	// Signing secret embedded in the cached struct, so invalidate.
	bumpDispatchCacheVersion()

	return secret, nil
}

// GetWebhook retrieves a webhook by ID.
func GetWebhook(ctx context.Context, webhookId uuid.UUID) (*webhookModel.Webhook, error) {
	return domain.GetWebhookById(ctx, webhookId)
}

// GetAllWebhooks retrieves all active webhooks.
func GetAllWebhooks(ctx context.Context) ([]*webhookModel.Webhook, error) {
	return domain.GetAllWebhooks(ctx)
}

// GetWebhookLogs retrieves paginated logs for a webhook.
func GetWebhookLogs(ctx context.Context, webhookId uuid.UUID, page int, pageSize int) ([]*logModel.WebhookLog, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize
	return domain.GetWebhookLogs(ctx, webhookId, pageSize, offset)
}

// ProcessIncomingWebhook validates the token and returns the webhook info.
// The actual message creation is handled by the controller using existing Post/Chat business logic.
func ProcessIncomingWebhook(ctx context.Context, token string) (*webhookModel.Webhook, error) {
	webhook, err := domain.GetWebhookByToken(ctx, token)
	if err != nil {
		return nil, err
	}

	if webhook == nil {
		return nil, fmt.Errorf("invalid webhook token")
	}

	if webhook.Type != "incoming" {
		return nil, fmt.Errorf("token is not for an incoming webhook")
	}

	if !webhook.IsActive {
		return nil, fmt.Errorf("webhook is disabled")
	}

	// Update last triggered
	now := time.Now()
	helpers.GoSafeNamed("business/Webhook/webhookBusiness:ProcessIncomingWebhook", func() {
		_ = domain.UpdateWebhookTriggerInfo(appCtx, webhook.Id, now, 0)
	})

	return webhook, nil
}

// DispatchEvent sends an event to all matching outgoing webhooks.
// It extracts scope context from event data (channel_id, project_id) to support scoped webhooks.
//
// Bulk-import suppression: when ctx carries helpers.BulkImportContextKey
// the function returns immediately without dispatching. Historical
// imports (Slack workspace ZIP, etc.) would otherwise fan out tens of
// thousands of `user.joined`/`post.created`/etc. events to every
// configured outbound webhook, which both spams the operator's
// destinations and exhausts the dispatcher's semaphore pool.
// Imported content is recorded directly to PG/Dgraph without any need
// to notify external systems retroactively.
func DispatchEvent(ctx context.Context, eventType string, data interface{}) {
	dispatch(ctx, eventType, data, nil)
}

// DispatchEventFor is DispatchEvent for an event only some people may know
// of: an outgoing webhook is sent it only when mayKnow says the person who set
// the webhook up may. A doc's events go this way (business/Follow
// webhooks.go), so a private doc's never reach a webhook whose owner can't
// open the doc. In-process listeners, which act inside the workspace, are
// told as of any event.
func DispatchEventFor(ctx context.Context, eventType string, data map[string]interface{}, mayKnow func(ctx context.Context, owner uuid.UUID) bool) {
	dispatch(ctx, eventType, data, mayKnow)
}

// queueDeliveries hands the webhooks an event goes to, each with the body cut
// for it, to business/Hooks, which writes them down before anything is sent
// and sends them until they arrive; a seam for tests.
var queueDeliveries = hooksBusiness.Queue

func dispatch(ctx context.Context, eventType string, data interface{}, mayKnow func(ctx context.Context, owner uuid.UUID) bool) {
	if helpers.IsBulkImport(ctx) {
		return
	}

	// Fan out to in-process listeners (e.g. the Workflow engine) BEFORE the
	// outgoing-webhook path. Listeners are internal subscribers that react to
	// workspace events without an HTTP destination; they run regardless of
	// whether any outgoing webhook is configured. Registered at startup via
	// RegisterEventListener, so there is no import cycle (this package never
	// imports its subscribers).
	//
	// Writes produced BY the workflow engine carry the workflow-generated flag;
	// we skip listener fan-out for those so a workflow's reply can't trigger
	// another workflow in an infinite loop. Outgoing webhooks still fire.
	if m, ok := data.(map[string]interface{}); ok && !helpers.IsWorkflowGenerated(ctx) {
		notifyEventListeners(ctx, eventType, m)
	}

	// Where it happened, which decides who is sent what (outgoingPrivacy.go):
	// a DM's or a group chat's goes to no webhook at all.
	m, _ := data.(map[string]interface{})
	place := placeOf(eventType, m)
	if place.conversation {
		return
	}
	// A restricted task's events reach only the webhooks whose owners may see
	// the task (models/taskread), asked once per owner below.
	if task := taskOfEvent(m); task != "" && restrictedTaskEvent(ctx, m) {
		inner := mayKnow
		mayKnow = func(ctx context.Context, owner uuid.UUID) bool {
			if inner != nil && !inner(ctx, owner) {
				return false
			}
			f, err := taskread.For(ctx, owner.String())
			return err == nil && !f.Hides(task)
		}
	}
	lookupKind := place.kind
	if place.id == nil {
		lookupKind = ScopeOrg // a place whose id doesn't read has no webhooks of its own
	}

	webhooks, err := getActiveOutgoingWebhooksByEventCached(ctx, eventType, lookupKind, place.id)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DispatchEvent Failed to get outgoing webhooks for event %s err: %+v",
			eventType, err)
		return
	}

	if len(webhooks) == 0 {
		return
	}

	// Whether a channel event's channel is public, asked only when a
	// workspace webhook would be sent it. Not known is private.
	public := false
	if place.kind == ScopeChannel && place.id != nil {
		for _, w := range webhooks {
			if w.ScopeType == "" || w.ScopeType == ScopeOrg {
				ok, err := domain.ChannelIsPublic(ctx, *place.id)
				public = err == nil && ok
				break
			}
		}
	}

	// The two payloads a webhook may be sent: the event whole, or its ids,
	// each with the event's id, the same on every try.
	eventID := uuid.New()
	payloadFor := func(body interface{}) ([]byte, error) {
		return json.Marshal(OutgoingWebhookPayload{
			Event:     eventType,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Data:      body,
			EventID:   eventID.String(),
		})
	}
	allBytes, err := payloadFor(data)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DispatchEvent Failed to marshal payload err: %+v",
			err)
		return
	}
	idBytes, err := payloadFor(idsOnly(m))
	if err != nil {
		return
	}

	// Extract plain text for trigger word matching
	var plainText string
	if pt, ok := m["plain_text"].(string); ok {
		plainText = pt
	} else if t, ok := m["text"].(string); ok {
		plainText = t
	}

	// Whether each webhook's owner may know of the event, asked once per owner.
	knows := map[uuid.UUID]bool{}
	var targets []hooksBusiness.Target
	for _, webhook := range webhooks {
		if mayKnow != nil {
			ok, asked := knows[webhook.CreatedBy]
			if !asked {
				ok = mayKnow(ctx, webhook.CreatedBy)
				knows[webhook.CreatedBy] = ok
			}
			if !ok {
				continue
			}
		}
		switch deliveryTo(webhook.ScopeType, webhook.ScopeEntityId, place, public) {
		case sendNothing:
			continue
		case sendIDs:
			// Trigger words would be read in the words the webhook isn't
			// sent: one that has some isn't told of the event at all, or it
			// would learn which private messages say "salary".
			if webhook.TriggerWords != nil && *webhook.TriggerWords != "" && *webhook.TriggerWords != "[]" {
				continue
			}
			targets = append(targets, hooksBusiness.Target{WebhookID: webhook.Id, Payload: idBytes})
			continue
		}
		// Trigger word filtering with word boundaries (cached compiled regexes)
		if webhook.TriggerWords != nil && *webhook.TriggerWords != "" && plainText != "" {
			var words []string
			if err := json.Unmarshal([]byte(*webhook.TriggerWords), &words); err == nil && len(words) > 0 {
				regexes := getCachedTriggerRegexes(webhook.Id, words)
				matched := false
				lowerPlain := strings.ToLower(plainText)
				for _, re := range regexes {
					if re.MatchString(lowerPlain) {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
		}
		targets = append(targets, hooksBusiness.Target{WebhookID: webhook.Id, Payload: allBytes})
	}
	if err := queueDeliveries(ctx, eventID, eventType, targets); err != nil {
		helpers.LogErrorWithContext(ctx, "business/DispatchEvent %s reached no webhook: %v", eventType, err)
	}
}

// triggerRegexCacheEntry holds compiled regexes plus a content hash for safe invalidation.
type triggerRegexCacheEntry struct {
	wordsHash string
	regexes   []*regexp.Regexp
}

// hashWords returns a deterministic hash of a word slice.
func hashWords(words []string) string {
	h := sha256.New()
	for _, w := range words {
		h.Write([]byte(w))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// getCachedTriggerRegexes returns compiled regexes for a webhook's trigger words,
// caching them per webhook ID and validating with a content hash.
func getCachedTriggerRegexes(webhookID uuid.UUID, words []string) []*regexp.Regexp {
	key := webhookID.String()
	wh := hashWords(words)
	if cached, ok := triggerWordRegexCache.Load(key); ok {
		entry := cached.(*triggerRegexCacheEntry)
		if entry.wordsHash == wh {
			return entry.regexes
		}
	}
	regexes := make([]*regexp.Regexp, 0, len(words))
	for _, word := range words {
		if re := helpers.KeywordPattern(word); re != nil {
			regexes = append(regexes, re)
		}
	}
	triggerWordRegexCache.Store(key, &triggerRegexCacheEntry{wordsHash: wh, regexes: regexes})
	return regexes
}

// evictTriggerRegexCache removes a webhook's cached trigger regexes.
func evictTriggerRegexCache(webhookID uuid.UUID) {
	triggerWordRegexCache.Delete(webhookID.String())
}

// TestWebhook sends a test event to a webhook.
func TestWebhook(ctx context.Context, webhookId uuid.UUID) error {
	webhook, err := domain.GetWebhookById(ctx, webhookId)
	if err != nil || webhook == nil {
		return fmt.Errorf("webhook not found")
	}

	if webhook.Type == "incoming" {
		return fmt.Errorf("cannot test incoming webhooks — send a POST request to the webhook URL")
	}

	testData := map[string]interface{}{
		"test":    true,
		"message": "This is a test event from OneCamp",
	}

	eventID := uuid.New()
	payload := OutgoingWebhookPayload{
		Event:     "test",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      testData,
		EventID:   eventID.String(),
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal test payload")
	}

	return queueDeliveries(ctx, eventID, "test", []hooksBusiness.Target{{WebhookID: webhook.Id, Payload: payloadBytes}})
}

// GetWebhookLogsGlobal retrieves paginated logs across all webhooks with filters.
func GetWebhookLogsGlobal(ctx context.Context, webhookId *uuid.UUID, success *bool, fromDate *time.Time, toDate *time.Time, page int, pageSize int) ([]*logModel.WebhookLog, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize
	return domain.GetWebhookLogsGlobal(ctx, webhookId, success, fromDate, toDate, pageSize, offset)
}

// CleanupOldLogs removes logs older than the retention period.
func CleanupOldLogs(ctx context.Context) (int64, error) {
	return domain.CleanupOldWebhookLogs(ctx, DefaultLogRetentionDays)
}

// ProcessIncomingMessage posts an incoming-webhook message into the target
// channel AS the shared automation bot, badged with the webhook's bot_name.
//
// Authorization is still anchored to the human who created the webhook: that
// user must be a member/admin of the target channel. Once authorized, the
// actual write goes through the shared bot identity (botpost.PostToChannel) so
// every automated message — webhook or workflow — carries one consistent,
// recognizable bot author instead of impersonating the creator's account.
func ProcessIncomingMessage(ctx context.Context, webhook *webhookModel.Webhook, channelUUID uuid.UUID, text string, botName string) (string, error) {
	// Authorization: resolve the creator and confirm channel access.
	userDgraph, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, webhook.CreatedBy.String())
	if err != nil || userDgraph == nil {
		helpers.LogErrorWithContext(ctx,
			"business/ProcessIncomingMessage Failed to get user dgraph info err: %+v", err)
		return "", fmt.Errorf("webhook creator user not found")
	}
	channelDgraph, err := channelDomain.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID.String(), userDgraph.Uid)
	if err != nil || channelDgraph == nil {
		helpers.LogErrorWithContext(ctx,
			"business/ProcessIncomingMessage Failed to get channel dgraph info err: %+v", err)
		return "", fmt.Errorf("channel not found")
	}
	if channelDgraph.IsMember == 0 && channelDgraph.IsAdmin == 0 {
		return "", fmt.Errorf("webhook creator is not a member of this channel")
	}

	// Post as the shared automation bot, labelled with the webhook's bot name,
	// the default for a webhook stored before names were trimmed.
	botName = botNameOf(botName, defaultBotName)
	res, err := botpost.PostToChannel(ctx, channelUUID, text, botName)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/ProcessIncomingMessage post-as-bot failed err: %+v", err)
		return "", err
	}

	// Dispatch the outgoing webhook event (post.created) so external
	// subscribers still see the message. In-process listeners (the workflow
	// engine) are NOT suppressed here intentionally: an incoming-webhook
	// message is a legitimate workspace message that a workflow may react to.
	//
	// It names no author: the shared bot posted it, and the words are whoever
	// holds the webhook's URL, or whatever wrote the alert or CI text it
	// relays. Not the webhook's creator, who wrote none of it. So an AI agent
	// the post reaches acts for nobody identified (business/AIAgent).
	helpers.GoSafeNamed("DispatchEvent", func() {
		DispatchEvent(appCtx, "post.created", map[string]interface{}{
			"post_id":    res.PostUUID,
			"channel_id": channelUUID.String(),
			"text":       res.HTMLText,
			"bot_name":   botName,
			"source":     "incoming_webhook",
		})
	})

	return res.PostUUID, nil
}

// VerifySignatureV1 validates the v1 HMAC-SHA256 signature with replay protection.
// Format: X-OneCamp-Signature: v1=<hex>  with  X-OneCamp-Timestamp header.
func VerifySignatureV1(secret string, body []byte, timestamp string, signature string) bool {
	if signature == "" || timestamp == "" {
		return false
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if time.Since(time.Unix(ts, 0)) > SignatureTolerance {
		return false
	}

	expectedSig := hooksBusiness.Sign(secret, timestamp, body)

	parts := strings.SplitN(signature, "=", 2)
	if len(parts) != 2 {
		return false
	}

	return hmac.Equal([]byte(expectedSig), []byte(parts[1]))
}

// VerifySignature validates the legacy HMAC-SHA256 signature of an incoming webhook request.
// Supports both OneCamp-style (X-OneCamp-Signature: sha256=...) without replay protection.
// Deprecated: use VerifySignatureV1 for production security.
func VerifySignature(secret string, body []byte, signatureHeader string) bool {
	if signatureHeader == "" {
		return false
	}

	// Parse signature format: "sha256=..."
	parts := strings.SplitN(signatureHeader, "=", 2)
	if len(parts) != 2 {
		return false
	}

	expectedSig := signPayload(secret, body)
	return hmac.Equal([]byte(expectedSig), []byte(parts[1]))
}

// VerifySlackSignature validates Slack-style request signatures.
// Format: v0=HMAC_SHA256(v0:timestamp:body, secret)
// Deprecated: prefer VerifySignatureV1 for new integrations.
func VerifySlackSignature(secret string, body []byte, timestamp string, signature string) bool {
	if signature == "" || timestamp == "" {
		return false
	}

	// Prevent replay attacks: reject requests older than 5 minutes
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if time.Since(time.Unix(ts, 0)) > 5*time.Minute {
		return false
	}

	baseString := fmt.Sprintf("v0:%s:%s", timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(baseString))
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	parts := strings.SplitN(signature, "=", 2)
	if len(parts) != 2 {
		return false
	}

	return hmac.Equal([]byte(expectedSig), []byte(parts[1]))
}

// taskOfEvent is the task an event's data names (task_id, task_uuid), or "".
func taskOfEvent(data map[string]interface{}) string {
	for _, key := range []string{"task_id", "task_uuid"} {
		if raw, _ := data[key].(string); strings.TrimSpace(raw) != "" {
			return strings.TrimSpace(raw)
		}
	}
	return ""
}
