package business

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/microcosm-cc/bluemonday"

	botpost "github.com/akashc777/OneCamp/business/BotPost"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	domain "github.com/akashc777/OneCamp/domain/Webhook"
	"github.com/akashc777/OneCamp/helpers"
	webhookModel "github.com/akashc777/OneCamp/models/postgres/Webhook"
	logModel "github.com/akashc777/OneCamp/models/postgres/WebhookLog"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

const (
	MaxFailureCount         = 10
	DefaultLogRetentionDays = 30
	OutgoingTimeout         = 10 * time.Second
	MaxResponseBodyLog      = 2048
	WebhookRateLimitPerMin  = 30
	SignatureVersionV1      = "v1"
	SignatureTolerance      = 5 * time.Minute
	MaxConcurrentDispatches = 100
)

var (
	// htmlPolicy is a shared UGC policy for sanitizing webhook-generated HTML.
	htmlPolicy = bluemonday.UGCPolicy()

	// webhookDispatchPool limits concurrent outgoing webhook dispatches.
	webhookDispatchPool = make(chan struct{}, MaxConcurrentDispatches)

	// sharedWebhookHTTPClient is reused across all outgoing webhook dispatches.
	// It uses SSRF-safe transport that pins resolved IPs at dial time.
	sharedWebhookHTTPClient = helpers.SSRFSafeClient(false)

	// triggerWordRegexCache maps webhook ID -> compiled regexes for trigger words.
	triggerWordRegexCache sync.Map

	// appCtx is the application-level context used by background goroutines.
	// It is set at startup via SetAppContext.
	appCtx = context.Background()
)

// Pre-compiled mrkdwn regexes for block rendering performance.
var (
	reMrkdwnBold     = regexp.MustCompile(`\*([^*]+)\*`)
	reMrkdwnItalic   = regexp.MustCompile(`_([^_]+)_`)
	reMrkdwnCode     = regexp.MustCompile("`([^`]+)`")
	reMrkdwnStrike   = regexp.MustCompile(`~([^~]+)~`)
	reMrkdwnLink     = regexp.MustCompile(`<([^|>]+)\|([^>]+)>`)
	reMrkdwnPlainURL = regexp.MustCompile(`<([^\s|>]+)>`)
	reHrefScheme     = regexp.MustCompile(`<a\s+href="([^"]*)"`)
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

// Block represents a Slack-style block for rich messages.
type Block struct {
	Type      string                 `json:"type"`
	Text      *BlockText             `json:"text,omitempty"`
	Fields    []BlockText            `json:"fields,omitempty"`
	Accessory map[string]interface{} `json:"accessory,omitempty"`
	ImageURL  string                 `json:"image_url,omitempty"`
	AltText   string                 `json:"alt_text,omitempty"`
	Title     *BlockText             `json:"title,omitempty"`
	Actions   []BlockAction          `json:"elements,omitempty"`
}

type BlockText struct {
	Type  string `json:"type"` // plain_text or mrkdwn
	Text  string `json:"text"`
	Emoji bool   `json:"emoji,omitempty"`
}

// BlockAction represents an interactive button or select in a block.
type BlockAction struct {
	Type     string     `json:"type"` // button, static_select, overflow
	Text     *BlockText `json:"text,omitempty"`
	ActionID string     `json:"action_id,omitempty"`
	URL      string     `json:"url,omitempty"`
	Value    string     `json:"value,omitempty"`
	Style    string     `json:"style,omitempty"` // primary, danger, default
}

// InteractivePayload is received when a user clicks an interactive block element.
type InteractivePayload struct {
	Type        string        `json:"type"`
	ActionID    string        `json:"action_id"`
	BlockID     string        `json:"block_id"`
	Value       string        `json:"value"`
	UserID      string        `json:"user_id"`
	ChannelID   string        `json:"channel_id"`
	Actions     []BlockAction `json:"actions"`
	ResponseURL string        `json:"response_url,omitempty"`
}

// IncomingWebhookPayload is the expected format for incoming webhook messages.
// Supports both simple text and Slack-style blocks for rich bot/AI messages.
// Destination is determined entirely from the payload (Slack-quality flexible routing):
//   - channel_id or channel → post to channel
//   - dm_id → direct message
//   - group_chat_id → group chat message
//   - none of the above → fallback to webhook's default channel_id
//
// The webhook's target_type is no longer used for routing.
type IncomingWebhookPayload struct {
	Text        string       `json:"text"`
	ChannelId   *string      `json:"channel_id,omitempty"`
	Channel     string       `json:"channel,omitempty"` // Slack-style alias for channel_id (UUID or #name)
	DmId        *string      `json:"dm_id,omitempty"`
	GroupChatId *string      `json:"group_chat_id,omitempty"`
	BotName     *string      `json:"bot_name,omitempty"`
	Blocks      []Block      `json:"blocks,omitempty"`
	Ephemeral   *bool        `json:"ephemeral,omitempty"` // If true, only visible to target user
	ThreadTs    *string      `json:"thread_ts,omitempty"` // For threading
	Markdown    *bool        `json:"markdown,omitempty"`  // Parse text as markdown
	Attachments []Attachment `json:"attachments,omitempty"`

	// Slash command fields
	Command     string `json:"command,omitempty"`
	CommandText string `json:"command_text,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	TeamID      string `json:"team_id,omitempty"`
	TriggerID   string `json:"trigger_id,omitempty"`
	ResponseURL string `json:"response_url,omitempty"`
}

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

	if input.BotName == "" {
		input.BotName = "Webhook Bot"
	}

	err = domain.CreateWebhook(ctx, id, input.Name, input.Description, input.Type, token, secret, input.TargetUrl, input.ChannelId, eventsJson, createdBy, input.BotName, input.BotAvatarUrl, nil, now)
	if err != nil {
		return nil, err
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

	if input.BotName == "" {
		input.BotName = existing.BotName
	}

	now := time.Now()

	// Reset failure_count when webhook is re-enabled
	if !existing.IsActive && input.IsActive {
		_ = domain.UpdateWebhookTriggerInfo(ctx, webhookId, now, 0)
	}

	err = domain.UpdateWebhook(ctx, webhookId, input.Name, input.Description, input.TargetUrl, input.ChannelId, eventsJson, input.IsActive, input.BotName, input.BotAvatarUrl, existing.Metadata, now)
	if err != nil {
		return nil, err
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
	go func() {
		_ = domain.UpdateWebhookTriggerInfo(appCtx, webhook.Id, now, 0)
	}()

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

	// Extract scope context from event data
	var scopeType string
	var scopeEntityId *uuid.UUID
	if m, ok := data.(map[string]interface{}); ok {
		if chID, ok := m["channel_id"].(string); ok && chID != "" {
			scopeType = "channel"
			id, _ := uuid.Parse(chID)
			scopeEntityId = &id
		} else if projID, ok := m["project_id"].(string); ok && projID != "" {
			scopeType = "project"
			id, _ := uuid.Parse(projID)
			scopeEntityId = &id
		}
	}
	if scopeType == "" {
		scopeType = "org"
	}

	webhooks, err := getActiveOutgoingWebhooksByEventCached(ctx, eventType, scopeType, scopeEntityId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DispatchEvent Failed to get outgoing webhooks for event %s err: %+v",
			eventType, err)
		return
	}

	if len(webhooks) == 0 {
		return
	}

	payload := OutgoingWebhookPayload{
		Event:     eventType,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      data,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DispatchEvent Failed to marshal payload err: %+v",
			err)
		return
	}

	// Extract plain text for trigger word matching
	var plainText string
	if m, ok := data.(map[string]interface{}); ok {
		if pt, ok := m["plain_text"].(string); ok {
			plainText = pt
		} else if t, ok := m["text"].(string); ok {
			plainText = t
		}
	}

	for _, webhook := range webhooks {
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
		go dispatchToWebhook(webhook, eventType, payloadBytes)
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
		re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(word) + `\b`)
		if err == nil {
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

// dispatchToWebhook sends the payload to a single outgoing webhook with retry logic.
func dispatchToWebhook(webhook *webhookModel.Webhook, eventType string, payloadBytes []byte) {
	// Acquire semaphore to limit concurrent dispatches
	webhookDispatchPool <- struct{}{}
	defer func() { <-webhookDispatchPool }()

	ctx, cancel := context.WithTimeout(appCtx, OutgoingTimeout)
	defer cancel()

	if webhook.TargetUrl == nil || *webhook.TargetUrl == "" {
		helpers.LogErrorWithContext(ctx,
			"business/dispatchToWebhook Webhook %s has no target URL", webhook.Id.String())
		return
	}

	// SSRF guard: validate target URL before dispatch
	if _, err := helpers.ValidateOutboundURL(*webhook.TargetUrl, false); err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/dispatchToWebhook SSRF blocked for webhook %s: %v", webhook.Id.String(), err)
		return
	}

	startTime := time.Now()
	maxRetries := 3
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			webhookRetriesTotal.WithLabelValues(eventType).Inc()
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, *webhook.TargetUrl, bytes.NewReader(payloadBytes))
		if err != nil {
			lastErr = err
			continue
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "OneCamp-Webhook/1.0")
		req.Header.Set("X-OneCamp-Event", eventType)
		req.Header.Set("X-OneCamp-Webhook-Id", webhook.Id.String())
		req.Header.Set("X-OneCamp-Delivery", uuid.New().String())
		req.Header.Set("X-OneCamp-Retry-Num", fmt.Sprintf("%d", attempt))

		if webhook.Secret != nil && *webhook.Secret != "" {
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			canonical := fmt.Sprintf("%s:%s:%s", SignatureVersionV1, ts, string(payloadBytes))
			signature := signPayload(*webhook.Secret, []byte(canonical))
			req.Header.Set("X-OneCamp-Signature", SignatureVersionV1+"="+signature)
			req.Header.Set("X-OneCamp-Timestamp", ts)
		}

		resp, err := sharedWebhookHTTPClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		respBodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBodyLog))
		resp.Body.Close()
		respBody := string(respBodyBytes)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			webhookDispatchesTotal.WithLabelValues(eventType, "success").Inc()
			webhookDispatchDuration.WithLabelValues(eventType).Observe(time.Since(startTime).Seconds())
			now := time.Now()
			_ = domain.UpdateWebhookTriggerInfo(ctx, webhook.Id, now, 0)
			logWebhookExecution(ctx, webhook.Id, eventType, payloadBytes, &resp.StatusCode, &respBody, nil, startTime)
			return
		}

		lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateString(respBody, 256))
	}

	// All retries exhausted
	webhookDispatchesTotal.WithLabelValues(eventType, "failure").Inc()
	webhookFailuresTotal.WithLabelValues(eventType).Inc()
	webhookDispatchDuration.WithLabelValues(eventType).Observe(time.Since(startTime).Seconds())
	incrementFailureAndMaybeDisable(ctx, webhook)
	logWebhookExecution(ctx, webhook.Id, eventType, payloadBytes, nil, nil, lastErr, startTime)
}

// incrementFailureAndMaybeDisable atomically increments the failure counter and auto-disables after MaxFailureCount.
func incrementFailureAndMaybeDisable(ctx context.Context, webhook *webhookModel.Webhook) {
	now := time.Now()

	newCount, err := domain.IncrementWebhookFailureCount(ctx, webhook.Id, now)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/incrementFailureAndMaybeDisable Failed to increment failure count err: %+v", err)
		return
	}

	if newCount >= MaxFailureCount {
		helpers.MessageLogs.InfoLog.Printf("Webhook %s auto-disabled after %d consecutive failures", webhook.Id.String(), newCount)
		_ = domain.DisableWebhook(ctx, webhook.Id, now)
		// Pulled out of dispatch immediately; no point continuing to
		// serve the cached entry that says is_active=true.
		bumpDispatchCacheVersion()
	}
}

// logWebhookExecution writes an execution log entry.
func logWebhookExecution(ctx context.Context, webhookId uuid.UUID, eventType string, requestBody []byte, responseStatus *int, responseBody *string, execErr error, startTime time.Time) {
	duration := int(time.Since(startTime).Milliseconds())
	success := execErr == nil && responseStatus != nil && *responseStatus >= 200 && *responseStatus < 300

	reqBodyStr := string(requestBody)

	var errMsg *string
	if execErr != nil {
		e := execErr.Error()
		errMsg = &e
	}

	_ = domain.CreateWebhookLog(ctx, webhookId, eventType, &reqBodyStr, responseStatus, responseBody, errMsg, &duration, success)
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

	payload := OutgoingWebhookPayload{
		Event:     "test",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      testData,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal test payload")
	}

	go dispatchToWebhook(webhook, "test", payloadBytes)
	return nil
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

	// Post as the shared automation bot, labelled with the webhook's bot name.
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
	go DispatchEvent(appCtx, "post.created", map[string]interface{}{
		"post_id":    res.PostUUID,
		"channel_id": channelUUID.String(),
		"text":       res.HTMLText,
		"bot_name":   botName,
		"source":     "incoming_webhook",
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

	canonical := fmt.Sprintf("%s:%s:%s", SignatureVersionV1, timestamp, string(body))
	expectedSig := signPayload(secret, []byte(canonical))

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

// SlashCommand represents a parsed slash command from an incoming webhook.
type SlashCommand struct {
	Command     string `json:"command"`
	Text        string `json:"text"`
	ResponseURL string `json:"response_url,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	ChannelID   string `json:"channel_id,omitempty"`
	TeamID      string `json:"team_id,omitempty"`
	TriggerID   string `json:"trigger_id,omitempty"`
}

// ParseSlashCommand extracts a slash command from incoming webhook text.
// Format: "/command arg1 arg2" or payload with explicit command field.
func ParseSlashCommand(payload IncomingWebhookPayload) *SlashCommand {
	// Explicit command field takes precedence
	if payload.Command != "" {
		chID := ""
		if payload.ChannelId != nil {
			chID = *payload.ChannelId
		}
		return &SlashCommand{
			Command:     payload.Command,
			Text:        payload.CommandText,
			ResponseURL: payload.ResponseURL,
			UserID:      payload.UserID,
			ChannelID:   chID,
			TeamID:      payload.TeamID,
			TriggerID:   payload.TriggerID,
		}
	}

	// Parse from text field: "/command args..."
	text := strings.TrimSpace(payload.Text)
	if !strings.HasPrefix(text, "/") {
		return nil
	}

	parts := strings.Fields(text)
	if len(parts) == 0 {
		return nil
	}

	cmd := parts[0]
	args := ""
	if len(parts) > 1 {
		args = strings.Join(parts[1:], " ")
	}

	chID := ""
	if payload.ChannelId != nil {
		chID = *payload.ChannelId
	}
	return &SlashCommand{
		Command:     cmd,
		Text:        args,
		ResponseURL: payload.ResponseURL,
		UserID:      payload.UserID,
		ChannelID:   chID,
		TeamID:      payload.TeamID,
	}
}

// HandleSlashCommand processes a slash command and returns an immediate response.
// If ResponseURL is set, the handler may also dispatch an async delayed response.
func HandleSlashCommand(ctx context.Context, webhook *webhookModel.Webhook, cmd *SlashCommand) (*SlashCommandResponse, error) {
	// Look up registered commands for this webhook
	registered := getRegisteredCommands(ctx, webhook)

	// Find matching command
	var matched *RegisteredCommand
	for _, rc := range registered {
		if rc.Command == cmd.Command {
			matched = &rc
			break
		}
	}

	if matched == nil {
		return &SlashCommandResponse{
			Text:         fmt.Sprintf("Unknown command: %s. Type `/help` for available commands.", cmd.Command),
			ResponseType: "ephemeral",
		}, nil
	}

	// If handler_url is set, forward to external handler
	if matched.HandlerURL != "" {
		go forwardSlashCommandToHandler(matched.HandlerURL, webhook, cmd)
		return &SlashCommandResponse{
			Text:         fmt.Sprintf("Processing `%s`...", cmd.Command),
			ResponseType: matched.ResponseType,
		}, nil
	}

	// Inline handlers for built-in commands
	switch cmd.Command {
	case "/help":
		return handleHelpCommand(registered), nil
	case "/status":
		return handleStatusCommand(ctx, webhook, cmd), nil
	default:
		return &SlashCommandResponse{
			Text:         fmt.Sprintf("Command `%s` is registered but has no inline handler.", cmd.Command),
			ResponseType: "ephemeral",
		}, nil
	}
}

// SlashCommandResponse is the immediate response to a slash command.
type SlashCommandResponse struct {
	Text            string  `json:"text"`
	Blocks          []Block `json:"blocks,omitempty"`
	ResponseType    string  `json:"response_type,omitempty"` // ephemeral | in_channel
	ReplaceOriginal bool    `json:"replace_original,omitempty"`
}

// RegisteredCommand is a command registered on a webhook.
type RegisteredCommand struct {
	Command      string `json:"command"`
	Description  string `json:"description"`
	HandlerURL   string `json:"handler_url,omitempty"`
	ResponseType string `json:"response_type"`
}

func getRegisteredCommands(ctx context.Context, webhook *webhookModel.Webhook) []RegisteredCommand {
	if webhook.Commands == nil || *webhook.Commands == "" {
		return defaultCommands()
	}

	var cmds []RegisteredCommand
	if err := json.Unmarshal([]byte(*webhook.Commands), &cmds); err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/getRegisteredCommands Failed to parse commands JSON for webhook %s: %+v", webhook.Id, err)
		return defaultCommands()
	}
	return cmds
}

func defaultCommands() []RegisteredCommand {
	return []RegisteredCommand{
		{Command: "/help", Description: "Show available commands", ResponseType: "ephemeral"},
		{Command: "/status", Description: "Show webhook status", ResponseType: "ephemeral"},
	}
}

func handleHelpCommand(registered []RegisteredCommand) *SlashCommandResponse {
	var sb strings.Builder
	sb.WriteString("*Available commands:*\n")
	for _, cmd := range registered {
		sb.WriteString(fmt.Sprintf("• `%s` — %s\n", cmd.Command, cmd.Description))
	}
	return &SlashCommandResponse{
		Text:         sb.String(),
		ResponseType: "ephemeral",
	}
}

func handleStatusCommand(ctx context.Context, webhook *webhookModel.Webhook, cmd *SlashCommand) *SlashCommandResponse {
	status := "Active"
	if !webhook.IsActive {
		status = "Disabled"
	}

	lastTriggered := "Never"
	if webhook.LastTriggeredAt != nil {
		lastTriggered = webhook.LastTriggeredAt.Format(time.RFC3339)
	}

	return &SlashCommandResponse{
		Text: fmt.Sprintf(
			"*Webhook Status*\n• Name: %s\n• Status: %s\n• Type: %s\n• Failures: %d\n• Last triggered: %s",
			webhook.Name, status, webhook.Type, webhook.FailureCount, lastTriggered,
		),
		ResponseType: "ephemeral",
	}
}

// forwardSlashCommandToHandler sends the command to an external handler URL asynchronously.
func forwardSlashCommandToHandler(handlerURL string, webhook *webhookModel.Webhook, cmd *SlashCommand) {
	ctx, cancel := context.WithTimeout(appCtx, OutgoingTimeout)
	defer cancel()

	// SSRF guard
	if _, err := helpers.ValidateOutboundURL(handlerURL, false); err != nil {
		helpers.LogErrorWithContext(ctx, "business/forwardSlashCommandToHandler SSRF blocked: %v", err)
		return
	}

	payload := map[string]interface{}{
		"command":      cmd.Command,
		"text":         cmd.Text,
		"user_id":      cmd.UserID,
		"channel_id":   cmd.ChannelID,
		"team_id":      cmd.TeamID,
		"trigger_id":   cmd.TriggerID,
		"response_url": cmd.ResponseURL,
		"webhook_id":   webhook.Id.String(),
		"webhook_name": webhook.BotName,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/forwardSlashCommandToHandler Failed to marshal payload err: %+v", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, handlerURL, bytes.NewReader(payloadBytes))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/forwardSlashCommandToHandler Failed to create request err: %+v", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "OneCamp-Webhook/1.0")

	client := helpers.SSRFSafeClient(false)
	client.Timeout = 10 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/forwardSlashCommandToHandler Failed to forward command err: %+v", err)
		return
	}
	defer resp.Body.Close()

	// If the handler returns a response and we have a response_url, forward it
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && cmd.ResponseURL != "" {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		go sendResponseURLCallback(ctx, cmd.ResponseURL, respBody)
	}
}

// sendResponseURLCallback sends an async response to a response_url.
func sendResponseURLCallback(ctx context.Context, responseURL string, payload []byte) {
	// SSRF guard
	if _, err := helpers.ValidateOutboundURL(responseURL, false); err != nil {
		helpers.LogErrorWithContext(ctx, "business/sendResponseURLCallback SSRF blocked: %v", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(payload))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/sendResponseURLCallback Failed to create request err: %+v", err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "OneCamp-Webhook/1.0")

	client := helpers.SSRFSafeClient(false)
	client.Timeout = 30 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/sendResponseURLCallback Failed to send callback err: %+v", err)
		return
	}
	defer resp.Body.Close()
}

func truncateString(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

// RenderBlocksToHTML converts Slack-style blocks to HTML for rich messages.
func RenderBlocksToHTML(blocks []Block) string {
	var sb strings.Builder
	for _, block := range blocks {
		switch block.Type {
		case "section":
			if block.Text != nil {
				if block.Text.Type == "mrkdwn" {
					sb.WriteString(renderMrkdwn(block.Text.Text))
				} else {
					sb.WriteString("<p>" + htmlPolicy.Sanitize(block.Text.Text) + "</p>")
				}
			}
			if len(block.Fields) > 0 {
				sb.WriteString("<div style='display:flex;gap:16px;flex-wrap:wrap'>")
				for _, field := range block.Fields {
					sb.WriteString("<div style='flex:1;min-width:150px'>")
					if field.Type == "mrkdwn" {
						sb.WriteString(renderMrkdwn(field.Text))
					} else {
						sb.WriteString("<p>" + htmlPolicy.Sanitize(field.Text) + "</p>")
					}
					sb.WriteString("</div>")
				}
				sb.WriteString("</div>")
			}
		case "divider":
			sb.WriteString("<hr/>")
		case "header":
			if block.Text != nil {
				sb.WriteString("<h3>" + htmlPolicy.Sanitize(block.Text.Text) + "</h3>")
			}
		case "context":
			if block.Text != nil {
				sb.WriteString("<small style='color:#666'>" + htmlPolicy.Sanitize(block.Text.Text) + "</small>")
			}
		case "image":
			alt := block.AltText
			if alt == "" {
				alt = "image"
			}
			sb.WriteString(fmt.Sprintf(`<img src="%s" alt="%s" style="max-width:100%%;border-radius:4px"/>`, htmlPolicy.Sanitize(block.ImageURL), htmlPolicy.Sanitize(alt)))
			if block.Title != nil {
				sb.WriteString(fmt.Sprintf("<figcaption style='font-size:12px;color:#666'>%s</figcaption>", htmlPolicy.Sanitize(block.Title.Text)))
			}
		case "actions":
			if len(block.Actions) > 0 {
				sb.WriteString("<div style='display:flex;gap:8px;flex-wrap:wrap;margin:8px 0'>")
				for _, action := range block.Actions {
					style := "background:#fff;border:1px solid #ccc;padding:6px 12px;border-radius:4px;cursor:pointer;text-decoration:none;color:#333"
					if action.Style == "primary" {
						style = "background:#007bff;border:1px solid #007bff;padding:6px 12px;border-radius:4px;cursor:pointer;text-decoration:none;color:#fff"
					} else if action.Style == "danger" {
						style = "background:#dc3545;border:1px solid #dc3545;padding:6px 12px;border-radius:4px;cursor:pointer;text-decoration:none;color:#fff"
					}
					label := action.ActionID
					if action.Text != nil {
						label = action.Text.Text
					}
					if action.URL != "" {
						sb.WriteString(fmt.Sprintf(`<a href="%s" style="%s">%s</a>`, htmlPolicy.Sanitize(action.URL), style, htmlPolicy.Sanitize(label)))
					} else {
						sb.WriteString(fmt.Sprintf(`<button style="%s">%s</button>`, style, htmlPolicy.Sanitize(label)))
					}
				}
				sb.WriteString("</div>")
			}
		}
	}
	return htmlPolicy.Sanitize(sb.String())
}

func renderMrkdwn(text string) string {
	// Convert Slack mrkdwn to basic HTML using pre-compiled regexes.
	text = reMrkdwnBold.ReplaceAllString(text, `<strong>$1</strong>`)
	text = reMrkdwnItalic.ReplaceAllString(text, `<em>$1</em>`)
	text = reMrkdwnCode.ReplaceAllString(text, `<code>$1</code>`)
	text = reMrkdwnStrike.ReplaceAllString(text, `<del>$1</del>`)
	text = reMrkdwnLink.ReplaceAllString(text, `<a href="$1">$2</a>`)
	text = reMrkdwnPlainURL.ReplaceAllString(text, `<a href="$1">$1</a>`)

	// Restrict link schemes to http, https, and mailto
	text = reHrefScheme.ReplaceAllStringFunc(text, func(match string) string {
		m := reHrefScheme.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		scheme := ""
		if u, err := url.Parse(m[1]); err == nil && u.Scheme != "" {
			scheme = strings.ToLower(u.Scheme)
		}
		if scheme == "http" || scheme == "https" || scheme == "mailto" {
			return match
		}
		return `<a href="#"`
	})

	return "<p>" + text + "</p>"
}

// SetAppContext sets the application-level context for background webhook operations.
// Call this once at application startup (e.g. from main or router setup).
func SetAppContext(ctx context.Context) {
	if ctx != nil {
		appCtx = ctx
	}
}
