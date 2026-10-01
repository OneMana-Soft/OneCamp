// Package registry is the single source of truth for every Redis key
// the BE stores, plus its TTL, datatype and human-readable purpose.
//
// Why this exists
// ---------------
// Before this file, Redis keys were defined ad-hoc across the codebase:
//   - Several controllers and middleware did fmt.Sprintf("ai:rate:%s", ...)
//     inline.
//   - TTLs were declared as scattered constants in domain packages with no
//     way to audit them.
//   - Two bugs were hiding in plain sight:
//     1) "rate-limit" rate-limit-prefix variants ("archive:rate_limit:run",
//     "ai:rate:", "auth:rate:", "webhook:ratelimit:") fragmented the
//     namespace and made tuning per-feature impossible.
//     2) Some keys had no TTL at all (DeleteCache was the only cleanup),
//     so a Redis crash during pattern-delete could leak keys forever.
//
// This file fixes that by declaring every key as a typed Spec value.
// Every read/write site goes through the typed Build method, so adding a
// new key now requires:
//  1. Add a Spec here with a clear doc-comment.
//  2. Use spec.Build(arg, ...) at the call site.
//
// The dotted Namespace is the prefix only; Build interpolates the
// segments separated by ":". This forces consistent shape and lets us
// list every key the BE owns by walking the Specs() registry.
//
// Datatype hint
// -------------
// Datatype is informational, intended for `*` operators inspecting
// `redis-cli MEMORY USAGE` output. The library doesn't enforce it; we
// rely on type-safe wrappers in models/redis/store for actual writes.
package registry

import (
	"strings"
	"time"
)

// Datatype is a human-readable hint for what kind of Redis structure
// a key holds. Not enforced; for documentation/audit only.
type Datatype string

const (
	DatatypeString    Datatype = "string"  // Plain string or JSON-encoded blob.
	DatatypeCounter   Datatype = "counter" // Integer with INCR semantics.
	DatatypeSortedSet Datatype = "zset"    // Sorted set (e.g. sliding-window rate limit).
	DatatypeHash      Datatype = "hash"    // Hash (HSET/HGET).
	DatatypeSet       Datatype = "set"     // Set.
)

// Category groups related keys for documentation and ops dashboards.
type Category string

const (
	CategoryAuth      Category = "auth"
	CategorySSO       Category = "sso"
	CategoryRateLimit Category = "ratelimit"
	CategoryUser      Category = "user"
	CategoryChannel   Category = "channel"
	CategoryProject   Category = "project"
	CategoryAI        Category = "ai"
	CategoryWebhook   Category = "webhook"
	CategoryImport    Category = "import"
	CategoryGitHub    Category = "github"
	CategoryCommand   Category = "command"
	CategoryCollab    Category = "collab"
)

// Spec describes a single Redis key namespace.
//
// The Namespace is a colon-separated prefix; Build appends each
// argument as an additional colon-separated segment. We chose ":" to
// stay aligned with Redis convention and most on-call tooling
// (RedisInsight, redis-cli scan).
//
// TTL of 0 means "no automatic expiry" — used for keys that are
// explicitly deleted (e.g. SSO state via GETDEL, refresh tokens
// rotated on logout). Every entry must opt-in to that explicitly so
// no key is silently long-lived by accident.
type Spec struct {
	// Namespace is the key prefix, e.g. "auth:refresh".
	Namespace string

	// TTL is the default TTL applied by store helpers when writing.
	// 0 means no TTL — caller must justify in Description.
	TTL time.Duration

	// Datatype is documentary; see Datatype constants.
	Datatype Datatype

	// Category groups specs for dashboards / docs.
	Category Category

	// Description is a one-paragraph "why this key exists" so an
	// on-call engineer can understand the Redis key from name alone.
	Description string

	// arity is the number of segments the spec expects from Build.
	// Computed lazily so we can validate at call time.
	arity int
}

// Build returns the fully-qualified key with the supplied segments
// joined by ":". A Spec with Namespace "auth:refresh" and arity 2
// receives e.g. (userId, deviceId) and produces
// "auth:refresh:<userId>:<deviceId>".
//
// Callers must pass the exact number of segments declared via
// `WithArity`; a mismatch is a programmer bug and panics at startup
// (caller-side test) rather than producing silently-wrong keys at
// runtime.
func (s Spec) Build(args ...string) string {
	if s.arity > 0 && len(args) != s.arity {
		// Panicking is appropriate: this is a structural mismatch
		// that any sensible test will catch immediately, and the
		// alternative (a silent fmt.Sprintf-style truncation) will
		// produce wrong-shape Redis keys that are very hard to
		// debug in production.
		panic("redis registry: spec " + s.Namespace + " expects " +
			itoa(s.arity) + " args, got " + itoa(len(args)))
	}
	if len(args) == 0 {
		return s.Namespace
	}
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, s.Namespace)
	parts = append(parts, args...)
	return strings.Join(parts, ":")
}

// Pattern returns a SCAN-friendly wildcard for invalidating all keys
// matching the given partial key. Does NOT enforce arity — callers
// may pass a subset of args to match broader patterns. Appends ":*"
// so `Pattern("ch1")` on a spec with arity 2 produces "ns:ch1:*".
func (s Spec) Pattern(args ...string) string {
	return s.partial(args...) + ":*"
}

// partial joins the namespace with the given args, separated by ":".
func (s Spec) partial(args ...string) string {
	if len(args) == 0 {
		return s.Namespace
	}
	return s.Namespace + ":" + strings.Join(args, ":")
}

// PatternAll returns "namespace:*" for ops to enumerate every entry.
func (s Spec) PatternAll() string {
	return s.Namespace + ":*"
}

// withArity returns a copy of the spec with the segment count
// validated; used inline in declarations to catch typos.
func withArity(s Spec, arity int) Spec {
	s.arity = arity
	return s
}

// itoa is a tiny stdlib-free int-to-ascii to avoid pulling in strconv
// just for the panic path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// =============================================================================
// Registry — every Redis key in the BE.
// =============================================================================
//
// Add new keys here, not in your business package.
// One declaration per key. Keep them in alphabetical order within their
// category.

// AUTH ------------------------------------------------------------------------

// UserRefreshToken stores the OPaque refresh token for a user/device pair.
// Persisted across restarts; TTL aligns with the auth refresh window.
var UserRefreshToken = withArity(Spec{
	Namespace:   "auth:refresh",
	TTL:         30 * 24 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategoryAuth,
	Description: "Per-user-per-device refresh token. Rotated on every refresh; deleted on logout.",
}, 2) // (userId, deviceId)

// LoginRate is a sliding-window login attempt counter per IP and
// surface (email vs. ldap). Backed by INCR + TTL window pin.
var LoginRate = withArity(Spec{
	Namespace:   "auth:rate",
	TTL:         15 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Login attempts per (surface, IP). Window-locked counter; trips after 20 hits.",
}, 2) // (surface, ip)

// SSO ------------------------------------------------------------------------

// SSOState is the one-shot OAuth/OIDC state nonce; stores the
// post-login redirect URL so the callback can recover it.
var SSOState = withArity(Spec{
	Namespace:   "sso:state",
	TTL:         5 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategorySSO,
	Description: "One-shot OAuth/OIDC state nonce → redirect URL. GETDEL on consume to prevent replay.",
}, 1) // (state)

// USER -----------------------------------------------------------------------

// UserProfile caches the postgres User row by UUID. Hot-path read on
// almost every authenticated request.
var UserProfile = withArity(Spec{
	Namespace:   "user:profile",
	TTL:         1 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategoryUser,
	Description: "JSON-encoded postgres User row. Invalidated on profile update.",
}, 1) // (userUUID)

// UserDgraphProfile caches the Dgraph user profile.
var UserDgraphProfile = withArity(Spec{
	Namespace:   "user:dgraph",
	TTL:         1 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategoryUser,
	Description: "JSON-encoded Dgraph user profile. Invalidated on profile update.",
}, 1) // (userUUID)

// UserSidebar caches the user's sidebar nav payload.
var UserSidebar = withArity(Spec{
	Namespace:   "user:sidebar",
	TTL:         10 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryUser,
	Description: "Sidebar nav payload. Invalidated on workspace membership change.",
}, 1) // (userUUID)

// UserChannels caches the user's channel list.
var UserChannels = withArity(Spec{
	Namespace:   "user:channels",
	TTL:         10 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryUser,
	Description: "Channel list for sidebar. Invalidated on channel membership change.",
}, 1) // (userUUID)

// UserProjects caches the user's project list.
var UserProjects = withArity(Spec{
	Namespace:   "user:projects",
	TTL:         10 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryUser,
	Description: "Project list for sidebar. Invalidated on project membership change.",
}, 1) // (userUUID)

// CHANNEL --------------------------------------------------------------------

// ChannelBasicInfo caches per-user-per-channel basic info (admin/member flags).
// Args: (channelUUID, userDgraphUid)
var ChannelBasicInfo = withArity(Spec{
	Namespace:   "channel:basic",
	TTL:         30 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryChannel,
	Description: "Per-user channel summary (admin/member flags). Invalidated on membership/admin change.",
}, 2) // (channelUUID, userDgraphUid)

// PROJECT --------------------------------------------------------------------

// ProjectTasks caches the kanban/list task fan-out per project.
// Multi-arg variants are stored under suffixes; we use the spec for
// invalidation patterns and the call site builds the full key.
var ProjectTasks = withArity(Spec{
	Namespace:   "project:tasks",
	TTL:         5 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryProject,
	Description: "Kanban/list task payload per project. Invalidated on task CRUD.",
}, 1) // (projectUUID)

// AI -------------------------------------------------------------------------

// AISession holds the multi-turn conversation state for an AI session.
var AISession = withArity(Spec{
	Namespace:   "ai:session",
	TTL:         30 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryAI,
	Description: "JSON-encoded AI conversation session. Refreshed on each turn.",
}, 1) // (sessionID)

// AIRate limits AI requests per minute per user.
var AIRate = withArity(Spec{
	Namespace:   "ai:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-user AI request counter. Window-locked; resets every minute.",
}, 1) // (userUUID)

// AIModelList caches a provider's fetched model catalog so the admin
// panel doesn't hit the provider API (or local Ollama) on every render.
// Invalidated implicitly by TTL and explicitly after a pull/delete.
var AIModelList = withArity(Spec{
	Namespace:   "ai:models",
	TTL:         5 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryAI,
	Description: "JSON model catalog per provider id. Short TTL; refreshed on pull/delete.",
}, 1) // (providerID)

// AIOllamaLatest caches the latest published Ollama version fetched from
// the GitHub releases API. Long TTL: unauthenticated GitHub allows only
// 60 req/hr/IP, and a new Ollama release is a once-a-week event at most.
var AIOllamaLatest = withArity(Spec{
	Namespace:   "ai:ollama:latest",
	TTL:         6 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategoryAI,
	Description: "Latest Ollama release tag from GitHub. Long TTL to respect GitHub rate limits.",
}, 0) // global (no args)

// AIAdminRate is the per-admin rate limiter for AI config actions that
// make outbound network calls (test connection, list models, pull). Keeps
// a misbehaving/compromised admin session from hammering external
// providers or a local Ollama. Action key encodes the operation.
var AIAdminRate = withArity(Spec{
	Namespace:   "ai:admin:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-admin AI config action rate limit (test/list/pull). Fixed window.",
}, 2) // (action, userUUID)

// AIOllamaCatalog caches the remote Ollama model-catalog manifest fetched
// from AI_OLLAMA_CATALOG_URL. Shared across instances so we don't refetch the
// hosted JSON on every admin render. Medium TTL: the list changes when new
// models ship (days/weeks), and the admin can force a refresh on demand.
var AIOllamaCatalog = withArity(Spec{
	Namespace:   "ai:ollama:catalog",
	TTL:         1 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategoryAI,
	Description: "Remote Ollama model catalog manifest JSON. Falls back to the embedded baseline.",
}, 0) // global (no args)

// AIWebSearch caches provider-agnostic web search results per (provider,
// query-hash) for a short window. Cuts cost + latency for metered providers
// (Tavily/Brave) and shields the provider from duplicate bursts (e.g. several
// agents asking the same thing), while a short TTL keeps "current" answers
// fresh enough.
var AIWebSearch = withArity(Spec{
	Namespace:   "ai:websearch",
	TTL:         10 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryAI,
	Description: "JSON web-search results per (provider, query hash). Short TTL.",
}, 2) // (provider, queryHash)

// AIUnifiedSearch caches a user's unified-search response per (userUUID,
// query-hash) for a short window. Unified search fans out to live external
// APIs (Gmail/GitHub) on every call, so a debounced search box would otherwise
// hit those APIs on each keystroke; a short TTL shields the providers and keeps
// repeat/refined searches snappy while staying fresh enough.
var AIUnifiedSearch = withArity(Spec{
	Namespace:   "ai:unifiedsearch",
	TTL:         60 * time.Second,
	Datatype:    DatatypeString,
	Category:    CategoryAI,
	Description: "JSON unified-search response per (user, query hash). Short TTL.",
}, 2) // (userUUID, queryHash)
// posts exactly one recap per finished call, even though room teardown
// can fire from both the room_finished and participant_left webhook paths.
var AIRecapLock = withArity(Spec{
	Namespace:   "ai:recap:lock",
	TTL:         30 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryAI,
	Description: "Per-room one-shot lock for the meeting recap agent (idempotency).",
}, 1) // (roomName)

// CallParticipants accumulates everyone who was in a call, so a post-call
// artifact can reach the people who attended rather than only the ones who
// spoke.
//
// Built from participant_left, which is the one event that fires for EVERY
// participant: a room cannot finish until they have all gone. room_finished
// carries the room and not its people, so without this the only record of who
// attended is the transcript, and the transcript only knows who talked.
//
// The TTL outlives a long meeting comfortably and is refreshed on every add, so
// it is measured from the last person to leave rather than the first.
var CallParticipants = withArity(Spec{
	Namespace:   "call:participants",
	TTL:         12 * time.Hour,
	Datatype:    DatatypeSet,
	Category:    CategoryAI,
	Description: "Dgraph uids of everyone who was in a call, keyed by room; read by the meeting recap.",
}, 1) // (roomName)

// CallUntranscribed records participants whose browser cannot transcribe them,
// so a transcript built from browser speech can say that it is incomplete.
//
// The Web Speech API exists in Chrome and Edge and nowhere else. In browser
// mode a participant on Firefox or Safari contributes nothing at all, and the
// result is not an error anywhere: the transcript simply has their turns
// missing, and a recap summarises a conversation it only heard half of. Silence
// and absence look identical in the record, which is the problem.
//
// Reported by the client, because the client is the only thing that knows. The
// server can see that someone spoke no lines, but cannot tell "said nothing"
// from "could not be heard", and guessing would put a caveat on every meeting
// with a quiet attendee.
var CallUntranscribed = withArity(Spec{
	Namespace:   "call:untranscribed",
	TTL:         12 * time.Hour,
	Datatype:    DatatypeSet,
	Category:    CategoryAI,
	Description: "Dgraph uids of call participants whose browser has no speech recognizer.",
}, 1) // (roomName)

// CallTranscriptGrant caches "this person is in this call, and here is the key
// their transcript lines belong under".
//
// Browser-mode transcription posts one request per utterance, and each one has
// to answer the same two questions: is the caller actually in this room, and
// which call is this. Asking LiveKit both on every line would be roughly a
// hundred API calls a minute for a five-person meeting.
//
// The short TTL is the security boundary. Someone who leaves the call can keep
// posting for at most this long, and the lines they could post in that window
// are lines they could have spoken a moment earlier, so the exposure is a
// minute of their own words under their own name.
var CallTranscriptGrant = withArity(Spec{
	Namespace:   "call:transcript:grant",
	TTL:         60 * time.Second,
	Datatype:    DatatypeString,
	Category:    CategoryAI,
	Description: "Verified call-session key per (room, participant) for browser-mode transcript posts.",
}, 2) // (roomName, participantIdentity)

// AIMemoryWatermark records the last unix-second timestamp the batched
// memory extractor processed for a given scope (channel/group). Content
// created after this point is what the next pass extracts. Long TTL so a
// quiet scope keeps its watermark; refreshed each pass.
var AIMemoryWatermark = withArity(Spec{
	Namespace:   "ai:memory:watermark",
	TTL:         90 * 24 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategoryAI,
	Description: "Last-processed unix ts for the batched memory extractor, per scope.",
}, 1) // (scopeKey)

// AIMemoryExtractLock guards a single extraction pass per scope so two
// worker ticks (or instances) can't double-extract the same window.
var AIMemoryExtractLock = withArity(Spec{
	Namespace:   "ai:memory:extract-lock",
	TTL:         10 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryAI,
	Description: "Per-scope one-shot lock for a batched memory extraction pass.",
}, 1) // (scopeKey)

// AIMemoryBackfillLock is a global one-shot lock ensuring only one admin
// memory-backfill job runs at a time (backfill is heavy: it sweeps all
// historical content). TTL bounds a crashed job so a later run isn't
// blocked forever.
var AIMemoryBackfillLock = withArity(Spec{
	Namespace:   "ai:memory:backfill-lock",
	TTL:         2 * time.Hour,
	Datatype:    DatatypeCounter,
	Category:    CategoryAI,
	Description: "Global one-shot lock for the admin memory-backfill job.",
}, 0) // global

// AIMemoryBackfillStatus holds the JSON progress/result of the most recent
// memory-backfill job so the admin panel can poll it. Survives the job so
// the UI can show the final summary; refreshed by the next run.
var AIMemoryBackfillStatus = withArity(Spec{
	Namespace:   "ai:memory:backfill-status",
	TTL:         24 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategoryAI,
	Description: "JSON status/result of the latest admin memory-backfill job.",
}, 0) // global

// AISelfTestLock is a global one-shot lock so only one admin AI self-test runs
// at a time (each test makes several real, possibly slow, model calls). TTL
// bounds a crashed run so a later one isn't blocked forever.
var AISelfTestLock = withArity(Spec{
	Namespace:   "ai:self-test-lock",
	TTL:         15 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryAI,
	Description: "Global one-shot lock for the admin AI self-test run.",
}, 0) // global

// AISelfTestStatus holds the JSON progress/result of the most recent admin AI
// self-test so the panel can poll it. Survives the run so the UI can show the
// final report; refreshed by the next run.
var AISelfTestStatus = withArity(Spec{
	Namespace:   "ai:self-test-status",
	TTL:         24 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategoryAI,
	Description: "JSON status/result of the latest admin AI self-test run.",
}, 0) // global

// AIMemoryDigestLock makes the proactive memory digest fire at most once
// per calendar day, even across overlapping hourly ticks and multiple
// instances. Keyed by YYYY-MM-DD; TTL comfortably exceeds the send window.
var AIMemoryDigestLock = withArity(Spec{
	Namespace:   "ai:memory:digest-lock",
	TTL:         23 * time.Hour,
	Datatype:    DatatypeCounter,
	Category:    CategoryAI,
	Description: "Per-day one-shot lock for the proactive memory digest run.",
}, 1) // (dayKey)

// AITeamReportLock makes the weekly team-report agent post at most once per
// (channel, period) across overlapping ticks/instances. Keyed by
// "<channelUUID>:<YYYY-MM-DD>"; TTL spans a few days to bridge the weekly
// cadence without leaking keys.
var AITeamReportLock = withArity(Spec{
	Namespace:   "ai:team-report:lock",
	TTL:         72 * time.Hour,
	Datatype:    DatatypeCounter,
	Category:    CategoryAI,
	Description: "Per-(channel,period) one-shot lock for the weekly team-report agent.",
}, 1) // (channelUUID:period)

// AINudgeRunLock makes the proactive-nudge engine run at most once per window
// across overlapping ticks and multiple instances. Keyed by a coarse time
// bucket so a crashed run can't permanently block the next one (TTL expiry).
var AINudgeRunLock = withArity(Spec{
	Namespace:   "ai:nudge:run-lock",
	TTL:         30 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryAI,
	Description: "One-shot lock per window for a proactive-nudge engine pass.",
}, 1) // (windowKey)

// AIAmbientCooldown rate-limits an ambient agent to at most one unprompted
// reply per (agent, channel) per window, so an opted-in agent can't flood a
// busy channel. The first INCR in a window returns 1 (proceed); a higher value
// means "still cooling down" (skip). Fail-safe: a Redis outage yields 0, which
// the caller treats as "skip", so ambient stays quiet when it can't be
// rate-limited.
var AIAmbientCooldown = withArity(Spec{
	Namespace:   "ai:ambient:cooldown",
	TTL:         5 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryAI,
	Description: "Per (agent, channel) cooldown for an ambient agent's unprompted replies.",
}, 2) // (agentID, channelID)

// TranscriptionAdminRate is the per-admin rate limiter for the transcription
// "test" action, which makes an outbound network probe to the configured STT
// provider. Bounds a misbehaving/compromised admin session from hammering an
// external endpoint. Fixed window, action-scoped like AIAdminRate.
var TranscriptionAdminRate = withArity(Spec{
	Namespace:   "transcription:admin:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-admin transcription config action rate limit (test). Fixed window.",
}, 2) // (action, userUUID)

// WebhookRateLimit is the sliding-window per-token rate limit for
// incoming webhook traffic.
var WebhookRateLimit = withArity(Spec{
	Namespace:   "webhook:rate",
	TTL:         2 * time.Minute,
	Datatype:    DatatypeSortedSet,
	Category:    CategoryRateLimit,
	Description: "Sliding-window incoming-webhook rate limit. ZADD timestamp; trim by score.",
}, 1) // (webhookID)

// ApiTokenRate is the per-token fixed-window rate limit for the public /v1
// API and the MCP server endpoint. Bounds how fast a single scoped token can
// drive the API, independent of the owner's interactive session. Fixed window,
// keyed by token id so each token gets its own bucket.
var ApiTokenRate = withArity(Spec{
	Namespace:   "apitoken:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-token public-API request counter. Fixed window; resets every minute.",
}, 1) // (tokenID)

// MCPToolReadRate bounds how fast one credential can READ through the MCP server.
//
// Separate from ApiTokenRate rather than reusing it, because the two surfaces fail
// differently. A human-written integration against /v1 makes a predictable number of
// calls; an agent loops, and a loop that re-reads the same document a thousand times
// is its NORMAL failure mode rather than an exceptional one. Giving MCP its own
// bucket means an agent misbehaving cannot exhaust the budget a person's integration
// depends on.
//
// Generous, because reads are cheap and an agent legitimately fans out over many
// documents to answer one question.
var MCPToolReadRate = withArity(Spec{
	Namespace:   "mcp:read:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-credential MCP read-tool counter. Fixed window; resets every minute.",
}, 1) // (tokenID)

// MCPToolWriteRate bounds MUTATING MCP calls, far more tightly than reads.
//
// Budgeted apart from reads on purpose. A runaway read loop wastes cycles; a runaway
// write loop produces a thousand messages in a channel, and no amount of after-the-
// fact auditing un-sends those. The cap is deliberately low enough that a human
// notices and intervenes before the damage is interesting, which is the only control
// that works against a mistake nobody predicted.
var MCPToolWriteRate = withArity(Spec{
	Namespace:   "mcp:write:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-credential MCP mutating-tool counter. Fixed window; resets every minute.",
}, 1) // (tokenID)

// ApiTokenTouch throttles the best-effort "last used" DB write to at most once
// per token per minute. Touching on every request would mean a DB UPDATE per
// API call (write amplification); this fixed-window-of-1 gate keeps the
// timestamp fresh without the per-request write.
var ApiTokenTouch = withArity(Spec{
	Namespace:   "apitoken:touch",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-token throttle for the last-used timestamp write. At most one DB touch per minute.",
}, 1) // (tokenID)
// day. EVERY AI completion (assistant chat, Board AI, agents, tables,
// workflows, ...) adds its consumed tokens here, and the provider layer refuses
// further calls once the configured daily budget is exceeded, bounding runaway
// AI cost across the whole workspace. Keyed by UTC day so it rolls over
// automatically; 24h TTL cleans it up.
var AITokenBudget = withArity(Spec{
	Namespace:   "ai:tokens:budget",
	TTL:         24 * time.Hour,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Workspace-wide AI token spend for the day. Gates all AI calls when over the configured daily budget.",
}, 1) // (dayKey YYYYMMDD)

// AIUserTokenBudget is the per-user AI token-spend counter for the current day,
// so one member can't burn the entire workspace budget. Mirrors AITokenBudget
// but keyed by (userUUID, dayKey); the provider layer increments both on every
// completion and gates a call when EITHER the user or the workspace cap is hit.
var AIUserTokenBudget = withArity(Spec{
	Namespace:   "ai:tokens:user",
	TTL:         24 * time.Hour,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-user AI token spend for the day. Gates that user's AI calls when over the configured per-user daily budget.",
}, 2) // (userUUID, dayKey YYYYMMDD)

// AITokenUserLeaderboard is a per-day sorted set of token spend keyed by user,
// so an admin can see today's top AI consumers without SCANning the per-user
// counter keyspace. RecordTokenSpend ZINCRBYs the actor here; the admin usage
// endpoint reads it with ZREVRANGE ... WITHSCORES. Keyed by UTC day, 24h TTL so
// it rolls over and self-cleans like the counters it summarizes.
var AITokenUserLeaderboard = withArity(Spec{
	Namespace:   "ai:tokens:leaderboard",
	TTL:         24 * time.Hour,
	Datatype:    DatatypeSortedSet,
	Category:    CategoryRateLimit,
	Description: "Per-day sorted set of AI token spend by user. Powers the admin 'top consumers today' view.",
}, 1) // (dayKey YYYYMMDD)

// AIAgentTokenBudget is the per-agent AI token-spend counter for the current
// day, so an AI teammate's autonomous work is capped on ITS OWN budget rather
// than burning its owner's personal quota. Keyed by (agentID, dayKey); the
// provider layer increments it on every agent completion and gates the run when
// the agent's configured daily cap is hit. 24h TTL self-cleans.
var AIAgentTokenBudget = withArity(Spec{
	Namespace:   "ai:tokens:agent",
	TTL:         24 * time.Hour,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-agent AI token spend for the day. Caps an AI teammate's autonomous spend independently of any human's quota.",
}, 2) // (agentID, dayKey YYYYMMDD)

// AIChannelTokenBudget is the per-channel AI token-spend counter for the
// current day (Claude-Tag-style channel-level cost control): all AI work
// attributed to a channel shares one daily cap. Keyed by (channelID, dayKey).
// 24h TTL self-cleans.
var AIChannelTokenBudget = withArity(Spec{
	Namespace:   "ai:tokens:channel",
	TTL:         24 * time.Hour,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-channel AI token spend for the day. Caps the AI cost incurred within a single channel.",
}, 2) // (channelID, dayKey YYYYMMDD)

// AIChannelTokenLeaderboard is a per-day sorted set of AI token spend keyed by
// channel, so an admin can see today's top AI-spending channels (Claude-Tag's
// per-channel usage breakdown) without SCANning the per-channel counters.
// Keyed by UTC day, 24h TTL so it rolls over and self-cleans.
var AIChannelTokenLeaderboard = withArity(Spec{
	Namespace:   "ai:tokens:channel:leaderboard",
	TTL:         24 * time.Hour,
	Datatype:    DatatypeSortedSet,
	Category:    CategoryRateLimit,
	Description: "Per-day sorted set of AI token spend by channel. Powers the admin 'top AI-spending channels today' view.",
}, 1) // (dayKey YYYYMMDD)

// IMPORT ---------------------------------------------------------------------

// ImportDiscover caches the per-provider/per-user discovery list
// (workspaces/boards visible to the connected token).
var ImportDiscover = withArity(Spec{
	Namespace:   "import:discover",
	TTL:         5 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryImport,
	Description: "Per-(provider,owner) discovery list. Dropped on connect/disconnect.",
}, 2) // (provider, ownerUserID)

// ImportRate is the per-user rate limiter for import-related admin actions.
var ImportRate = withArity(Spec{
	Namespace:   "import:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-user import action counter. Window-locked.",
}, 2) // (action, userUUID)

// SlackImportRate is a separate bucket for Slack-import admin actions.
var SlackImportRate = withArity(Spec{
	Namespace:   "slack-import:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-user Slack-import action counter. Window-locked.",
}, 2) // (action, userUUID)

// ArchiveRate is the per-user rate limiter for archive admin actions.
var ArchiveRate = withArity(Spec{
	Namespace:   "archive:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-user archive action counter. Window-locked.",
}, 2) // (action, userUUID)

// COMMAND --------------------------------------------------------------------

// CommandCatalog caches the resolved, scope-filtered command list per user so
// the composer typeahead never hits Postgres on the hot path. The cache key
// embeds the workspace catalog version (see CommandCatalogVersion) so an
// admin install/toggle/remove invalidates every user's cache via one bump.
var CommandCatalog = withArity(Spec{
	Namespace:   "command:catalog",
	TTL:         5 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryCommand,
	Description: "Per-user scoped slash-command catalog for the composer typeahead.",
}, 1) // (userUUID:version)

// CommandCatalogVersion is the workspace-wide catalog version token. Bumped on
// every admin change to the app/command registry; embedded in CommandCatalog
// cache keys so all users converge to the new catalog without per-key SCAN.
var CommandCatalogVersion = withArity(Spec{
	Namespace:   "command:catalog-version",
	TTL:         30 * 24 * time.Hour, // long-lived; on expiry reads fall back to "0" → one-time recompute
	Datatype:    DatatypeString,
	Category:    CategoryCommand,
	Description: "Workspace-wide slash-command catalog version token (cache buster).",
}, 0) // global

// CommandRate limits command executions per user to blunt abuse of external
// app dispatch and the scheduler.
var CommandRate = withArity(Spec{
	Namespace:   "command:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-user slash-command execution counter. Window-locked.",
}, 1) // (userUUID)

// CommandInteraction stores the transient state of an interactive card (e.g.
// the Giphy result set being shuffled) so a button click can resume it. Short
// TTL: cards are ephemeral and expire quickly.
var CommandInteraction = withArity(Spec{
	Namespace:   "command:interaction",
	TTL:         15 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryCommand,
	Description: "Transient interactive-card state keyed by trigger id.",
}, 1) // (triggerID)

// CommandOAuthState is the one-shot OAuth install state nonce for app
// authorization. GETDEL on consume to prevent replay (mirrors SSOState).
var CommandOAuthState = withArity(Spec{
	Namespace:   "command:oauth-state",
	TTL:         10 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryCommand,
	Description: "One-shot OAuth install state nonce → app id. GETDEL on consume.",
}, 1) // (state)

// ConnectorOAuthState is the one-shot OAuth state nonce for per-USER connector
// authorization (Gmail, GitHub, Calendar). The stored value binds the nonce to
// the initiating user + provider so the callback can only ever write that
// user's token. GETDEL on consume to prevent replay.
var ConnectorOAuthState = withArity(Spec{
	Namespace:   "connector:oauth-state",
	TTL:         10 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryCommand,
	Description: "One-shot per-user connector OAuth state nonce → userUUID:provider. GETDEL on consume.",
}, 1) // (state)

// ConnectorBriefingDay caches the cross-connector "Your day" agenda
// (calendar/PRs/email) per user. Short TTL keeps it fresh enough for a morning
// briefing while ensuring repeated home-screen loads don't hammer the external
// Gmail/Calendar/GitHub APIs on every mount.
var ConnectorBriefingDay = withArity(Spec{
	Namespace:   "connector:briefing-day",
	TTL:         2 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryCommand,
	Description: "Per-user cached cross-connector 'Your day' briefing agenda.",
}, 1) // (userUUID)

// COLLAB ---------------------------------------------------------------------

// CollabStateSize caches the byte-size of the last persisted state for a
// collaborative resource (board/doc). The snapshot subsystem reads this cheap
// value on every debounced save to detect a sharp shrink (mass-delete) without
// fetching the full prior state from dgraph on the hot persist path; the full
// prior state is read only when a shrink is actually suspected.
var CollabStateSize = withArity(Spec{
	Namespace:   "collab:state-size",
	TTL:         24 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategoryCollab,
	Description: "Last persisted state byte-size per (kind, id) for cheap mass-delete detection on save.",
}, 2) // (kind, id)

// =============================================================================
// Specs() returns every registered key for documentation / audit. Used
// by tests to assert no two specs share the same Namespace and by the
// admin debug endpoint to surface a "what does Redis hold" view.
// =============================================================================

// All returns the master list. Callers should treat the slice as
// read-only.
func All() []Spec {
	return []Spec{
		UserRefreshToken,
		LoginRate,
		SSOState,
		UserProfile,
		UserDgraphProfile,
		UserSidebar,
		UserChannels,
		UserProjects,
		ChannelBasicInfo,
		ProjectTasks,
		AISession,
		AIRate,
		AIModelList,
		AIOllamaLatest,
		AIWebSearch,
		AIUnifiedSearch,
		AIAdminRate,
		AIRecapLock,
		AIMemoryWatermark,
		AIMemoryExtractLock,
		AIMemoryBackfillLock,
		AIMemoryBackfillStatus,
		AISelfTestLock,
		AISelfTestStatus,
		AIMemoryDigestLock,
		AITeamReportLock,
		AINudgeRunLock,
		TranscriptionAdminRate,
		WebhookRateLimit,
		ApiTokenRate,
		MCPToolReadRate,
		MCPToolWriteRate,
		ApiTokenTouch,
		AITokenBudget,
		AIUserTokenBudget,
		AITokenUserLeaderboard,
		AIAgentTokenBudget,
		AIChannelTokenBudget,
		AIChannelTokenLeaderboard,
		ImportDiscover,
		ImportRate,
		SlackImportRate,
		ArchiveRate,
		CommandCatalog,
		CommandCatalogVersion,
		CommandRate,
		CommandInteraction,
		CommandOAuthState,
		ConnectorOAuthState,
		ConnectorBriefingDay,
		CollabStateSize,
	}
}
