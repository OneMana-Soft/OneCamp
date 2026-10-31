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
	CategorySweep     Category = "sweep"
	CategoryUnfurl    Category = "unfurl"
	CategoryCall      Category = "call"
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

// UserRefreshTokenPrev is a device's refresh token from before its last
// rotation, accepted until the device next rotates or for ten minutes: a tab
// that sent it while another tab refreshed, or a device whose refresh answer
// was lost on the way, gets the current one rather than being taken for a
// stolen copy. (A minute signed out anyone whose answer went missing.)
var UserRefreshTokenPrev = withArity(Spec{
	Namespace:   "auth:refresh-prev",
	TTL:         10 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryAuth,
	Description: "A device's refresh token before its last rotation, accepted for ten minutes so concurrent tabs and lost answers don't sign it out.",
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

// PublicReadRate counts reads of a page anyone may open (a booking page's free
// times, a guest's booking link), per address and per page, in a one-minute
// window. LoginRate counts only writes; these reads cost the server, and a
// booking page's cost its owner's Google quota, so every GET is counted.
var PublicReadRate = withArity(Spec{
	Namespace:   "public:read:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Public page reads per (surface, address or page). Fixed one-minute window.",
}, 2) // (surface, ip or a hash of the page)

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

// SAMLAssertionUsed marks a SAML assertion as used, so a captured answer
// can't sign anyone in again. services/SAML TakeAssertion keeps each key until
// MaxIssueDelay after its assertion's IssueInstant, when the library refuses
// the assertion anyway: this TTL, from the moment the IdP issued it.
var SAMLAssertionUsed = withArity(Spec{
	Namespace:   "sso:saml:used",
	TTL:         90 * time.Second,
	Datatype:    DatatypeString,
	Category:    CategorySSO,
	Description: "A SAML assertion ID already used to sign in. SET NX until the assertion expires; a second use is refused.",
}, 1) // (assertion id)

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
// Args: (channelUUID, userDgraphUid). Written with store.SetJSONIndexed and
// dropped per channel with store.DeleteIndexed, through ChannelBasicInfoKeys:
// "channel:basic" was dropped with a SCAN of the whole keyspace on every
// membership or admin change. The namespace moved with it, so a key written
// before the index (never recorded in one) is simply never read again and
// expires within its TTL.
var ChannelBasicInfo = withArity(Spec{
	Namespace:   "channel:basic2",
	TTL:         30 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryChannel,
	Description: "Per-user channel summary (admin/member flags). Invalidated on membership/admin change.",
}, 2) // (channelUUID, userDgraphUid)

// ChannelBasicInfoKeys is the set of users (their graph uids) a channel's
// ChannelBasicInfo is cached for, so the channel's entries are dropped by
// name. Args: (channelUUID). Its TTL is ChannelBasicInfo's, renewed on every add.
var ChannelBasicInfoKeys = withArity(Spec{
	Namespace:   "channel:basic2-keys",
	TTL:         30 * time.Minute,
	Datatype:    DatatypeSet,
	Category:    CategoryChannel,
	Description: "Which users a channel's basic-info cache holds entries for, so a membership change drops them without a SCAN.",
}, 1) // (channelUUID)

// PROJECT --------------------------------------------------------------------

// AI -------------------------------------------------------------------------

// AISession holds the multi-turn conversation state for an AI session.
var AISession = withArity(Spec{
	Namespace:   "ai:session",
	TTL:         30 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryAI,
	Description: "JSON-encoded AI conversation session. Refreshed on each turn.",
}, 1) // (sessionID)

// AIStreamStop records that the person a streamed answer belongs to asked for
// it to stop. Written by the stop endpoint, read by the handler that owns the
// stream when its client goes away, so a deliberate stop can be told apart from
// a tab that closed. The value is the requester's user uuid, so only the owner's
// request counts. Short TTL: a stop that nobody consumed within minutes was for
// a stream that already ended.
var AIStreamStop = withArity(Spec{
	Namespace:   "ai:stream:stop",
	TTL:         5 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryAI,
	Description: "Owner uuid; present while a deliberate stop of this session's answer is pending.",
}, 1) // (sessionID)

// AIStreamLive marks a session whose answer is still being written on the
// server after the client that asked has gone. A client that comes back reads
// it to know there is something to wait for. TTL is the stream timeout's order,
// refreshed while generation runs; a key that outlives its stream only makes a
// client poll a little longer.
var AIStreamLive = withArity(Spec{
	Namespace:   "ai:stream:live",
	TTL:         10 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryAI,
	Description: "Set while an answer for this session is being generated; cleared when it is saved.",
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
// AIRecapLock posts exactly one recap per finished call, even when the
// room_finished webhook is delivered twice.
//
// Keyed by the CALL (its session key), not the room. A channel's room is the
// same for every call in it, so a room lock held for 30 minutes meant a second
// call ending within 30 minutes of the first got no recap at all.
var AIRecapLock = withArity(Spec{
	Namespace:   "ai:recap:lock",
	TTL:         30 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryAI,
	Description: "Per-call one-shot lock for the meeting recap agent (idempotency).",
}, 1) // (callSessionKey)

// CallAttendees is everyone who joined ONE call, so its notes reach the people
// who attended rather than only the ones who spoke.
//
// Keyed by the call's session key, never the room. It was keyed by the room
// with a 12-hour TTL refreshed on every leave, and a channel's room is the
// same for every call in it, so the set gathered everyone from every call in
// the channel that day: an afternoon call's notes were shared, with edit
// access, with the morning's attendees, including someone removed from the
// channel in between. Built from participant_joined (and participant_left as
// a backstop). Read at write time and re-checked against who can still open
// the conversation.
var CallAttendees = withArity(Spec{
	Namespace:   "call:attendees",
	TTL:         12 * time.Hour,
	Datatype:    DatatypeSet,
	Category:    CategoryCall,
	Description: "Dgraph uids of everyone who joined one call (by session key); read by the meeting notes.",
}, 1) // (callSessionKey)

// CallState is what a call has switched on, kept by the server: the
// transcription switch (who turned it on, when, and where the audio goes) and
// the recording running in it.
//
// The room's LiveKit metadata carries the same thing to every client, but
// the server needs an answer it can read on every transcript line without a
// LiveKit round trip, and one that a stale call can't leak into the next: the
// state names the room session it belongs to, and a newer session ignores it.
// Deleted when the room finishes; the TTL is the backstop for a lost webhook.
var CallState = withArity(Spec{
	Namespace:   "call:state",
	TTL:         12 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategoryCall,
	Description: "JSON switches of the call in a room (transcription, recording), with the room session they belong to.",
}, 1) // (roomName)

// CallRemoved is who a host put out of the call in a room, so they can't mint
// a new token and walk straight back in, and a still-valid token is thrown out
// again when it connects. Deleted when the room finishes.
var CallRemoved = withArity(Spec{
	Namespace:   "call:removed",
	TTL:         12 * time.Hour,
	Datatype:    DatatypeSet,
	Category:    CategoryCall,
	Description: "Identities removed from the call in a room, until the call ends.",
}, 1) // (roomName)

// TranscriptionAgentHeartbeat is the server transcriber saying it is running.
//
// Server-mode transcription is done by the Python agent, and a workspace can
// choose it, including "audio never leaves this server", on an install where
// no agent runs: every call then had no captions and no transcript, and the
// admin's Test passed, because it probed the speech model and not the agent.
// The agent posts this every 30 seconds; the admin card and Test read it.
var TranscriptionAgentHeartbeat = withArity(Spec{
	Namespace:   "transcription:agent:heartbeat",
	TTL:         90 * time.Second,
	Datatype:    DatatypeString,
	Category:    CategoryCall,
	Description: "JSON last heartbeat of the transcription agent (time, LiveKit reachable).",
}, 0)

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
//
// Keyed by the call's session key: keyed by the room, a caveat from a morning
// call reached the recap of every later call in the channel that day.
var CallUntranscribed = withArity(Spec{
	Namespace:   "call:untranscribed",
	TTL:         12 * time.Hour,
	Datatype:    DatatypeSet,
	Category:    CategoryAI,
	Description: "Dgraph uids of call participants whose browser has no speech recognizer (by call session key).",
}, 1) // (callSessionKey)

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

// InviterDailyInvites counts the invitations one member made or sent again
// in the last day (controllers/User AddInvitation). Fixed window.
var InviterDailyInvites = withArity(Spec{
	Namespace:   "invite:inviter:day",
	TTL:         24 * time.Hour,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-member invitation counter: invitations a member made in the last day. Fixed window.",
}, 1) // (inviterUUID)

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

// ApiRouteRate is the public API's limit per token and route (platform P-API;
// design/platform/API_CONTRACT.md §3.2), beside ApiTokenRate's limit per
// token: a route registered with business/PublicAPI gets its own bucket, so a
// loop on one route can't spend what a token's other calls need, and writes
// are held tighter than reads.
var ApiRouteRate = withArity(Spec{
	Namespace:   "apiroute:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-token, per-route public-API request counter. Fixed window; resets every minute.",
}, 2) // (tokenID, route)

// ApiIdempotency is a write's answer kept under the Idempotency-Key it was
// sent with (platform P-API, §3.2): the same key again with the same body is
// answered from here, never run twice; with another body it is refused.
var ApiIdempotency = withArity(Spec{
	Namespace:   "apiroute:idem",
	TTL:         24 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategoryRateLimit,
	Description: "A write's answer under its Idempotency-Key (the public API's writes, a doc import), replayed for a retry with the same body.",
}, 3) // (caller: an API token's id or member:<uuid>, route, key)

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
// GuestAccessAudit lets the "a guest opened a shared ..." audit row be
// written at most once per link (and guest name) per half hour. A guest's
// channel and project pages refresh every few seconds, and auditing every
// refresh wrote about 720 rows an hour for one open tab.
var GuestAccessAudit = withArity(Spec{
	Namespace:   "guest:access:audit",
	TTL:         30 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-link throttle for the guest access audit row. At most one per half hour.",
}, 2) // (grantID, guest name or "")

// GuestNotifyOnce lets what guests write through a link tell the team at
// most once per channel, thread or doc per five minutes: a client writing a
// dozen short messages woke every member a dozen times.
var GuestNotifyOnce = withArity(Spec{
	Namespace:   "guest:notify:once",
	TTL:         5 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Per-target throttle for notifications of guests' words. At most one per five minutes per channel, thread or doc.",
}, 2) // (what: channel|thread|doc, its uuid)

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

// ImportOAuthState is one sign-in at an import provider in progress: the
// state handed to the provider, to whom and for which provider it was
// minted. GETDEL on the callback, so a state works once.
var ImportOAuthState = withArity(Spec{
	Namespace:   "import:oauth-state",
	TTL:         10 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryImport,
	Description: "One-shot import sign-in state nonce → provider and admin. GETDEL on the callback.",
}, 1) // (state)

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

// GiphyPage keeps one page of the composer's GIF picker (trending, or a
// search) for everyone, so people browsing the same GIFs spend the
// workspace's Giphy quota once.
var GiphyPage = withArity(Spec{
	Namespace:   "command:giphy-page",
	TTL:         10 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryCommand,
	Description: "A page of the GIF picker's Giphy results, by lowercased search and offset.",
}, 2) // (term, offset)

// CommandResponse is one command's response URL (business/Command): the app,
// who typed the command and where, and the answers it has left. It stops 30
// minutes after the command, whatever its TTL says after a use.
var CommandResponse = withArity(Spec{
	Namespace:   "command:response",
	TTL:         30 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryCommand,
	Description: "An app command's response URL: the invocation and its answers left (5 in 30 minutes).",
}, 1) // (token)

// CardTrigger is one press on a card (business/Cards): who pressed and whose
// card, for the owner to open a modal with, once. GETDEL on use; it lasts
// as long as a modal may take to open.
var CardTrigger = withArity(Spec{
	Namespace:   "cards:trigger",
	TTL:         3 * time.Second,
	Datatype:    DatatypeString,
	Category:    CategoryCommand,
	Description: "One-shot trigger of a press on a card: the presser and the card's owner. GETDEL on use.",
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

// PasskeyCeremony is one passkey sign-in or registration in progress: the
// challenge the server issued, read back once when the browser answers.
var PasskeyCeremony = withArity(Spec{
	Namespace:   "auth:passkey",
	TTL:         5 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryAuth,
	Description: "One-shot WebAuthn ceremony (challenge session JSON). GETDEL on finish.",
}, 1) // (ceremony id)

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

// TABLES ---------------------------------------------------------------------

// TablePresence is who has a table open, and the cell each has selected or
// is editing (tables A7, Live Rows #65): a hash, each person's id to their
// place and when they were last heard from. Someone not heard from in 45 s
// is gone and is dropped as it's read; the key goes when nobody has been
// heard from for its TTL.
var TablePresence = withArity(Spec{
	Namespace:   "table:presence",
	TTL:         2 * time.Minute,
	Datatype:    DatatypeHash,
	Category:    CategoryCollab,
	Description: "Who has a table open and the cell each has selected or is editing (tables A7). A field per person; entries older than 45 s are dropped on read.",
}, 1) // (tableUUID)

// TableRowList is the order of a view's rows that no index walks, listed
// once as row ids for its later windows to read slices of (tables T5d,
// business/RowQuery lists.go): 16 bytes an id. It is keyed by its view (a
// hash of the table, the spec and the reader), then a hash of what it was
// listed under (TableRowsToken, the fields, the reader's day) and where it
// starts, so a window reads only its own view's lists.
var TableRowList = withArity(Spec{
	Namespace:   "table:rowlist",
	TTL:         10 * time.Minute,
	Datatype:    DatatypeString,
	Category:    CategoryCollab,
	Description: "A view's row order listed once (tables T5d): row ids of 16 bytes each, read in slices by the view's later windows. Stale once its table's TableRowsToken changes.",
}, 2) // (view hash, list hash)

// TableRowsToken changes each time a write to a table's rows commits (the
// row ledger's observer), and each time the table's readers are told to look
// again because a linked table changed its links (business/RowQuery
// lists.go): a TableRowList made under another token is stale. A random
// token rather than a count, so a key that expired and began again can never
// repeat one a list holds; it outlives every list (24 hours against 10
// minutes), so a list made while there was none can't be read after a write.
var TableRowsToken = withArity(Spec{
	Namespace:   "table:rowstoken",
	TTL:         24 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategoryCollab,
	Description: "A random token replaced by each committed write to a table's rows, and each change to its links made from a linked table (tables T5d); the row lists made under another are stale.",
}, 1) // (tableUUID)

// ViewSalt is the day's random salt a published page's visitors are counted
// under (docs C6-3): one per UTC day, shared by every instance, gone two days
// on. Never logged; what it keys is never an address.
var ViewSalt = withArity(Spec{
	Namespace:   "views:salt",
	TTL:         48 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategoryCollab,
	Description: "The day's random salt for counting a published page's visitors, per UTC day; never logged.",
}, 1) // (day)

// SWEEPS ---------------------------------------------------------------------

// SweepClaim is one pass of a periodic job, won by the instance that runs it
// (business/Sweep): SET NX on the job's slot, the interval-long stretch of
// wall-clock time the tick falls in, so instances ticking in the same slot run
// the job once between them. The claim sets its own expiry, two of the job's
// intervals, so it outlives its slot; TTL is the longest of those today (the
// six-hourly sweeps). A job that claims its own work takes no claim. A wake
// published by a process that runs no sweeps (Registry.Wake) is claimed the
// same way, on "wake-" and the wake's id, for a minute, so one of the
// instances that hear it runs it.
var SweepClaim = withArity(Spec{
	Namespace:   "sweep:claim",
	TTL:         12 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategorySweep,
	Description: "One instance's claim on one pass of a periodic job (SET NX), a tick's slot or a wake's id; expires after two of the job's intervals (a minute for a wake), which the claim sets.",
}, 2) // (job, slot or "wake-" and the wake's id)

// SendOnce is a message send, by the person, the conversation ("ch:", "dm:" or
// "grp:" and its id) and the id their app gave it (business/Send.Once): a
// send under way holds it for a minute (SET NX), then its answer is kept for
// a day, so a send tried again gets the first one's answer.
var SendOnce = withArity(Spec{
	Namespace:   "send:once",
	TTL:         24 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategoryChannel,
	Description: "A message send tried again is the same send: \"going\" for a minute while the first try runs (SET NX), then its answer as JSON for a day.",
}, 3) // (user, conversation, client id)

// OutboxOnce is one sink having told one person of one outbox event (push, an
// Activity item: business/Outbox FirstTime): SET NX before each, so a retry
// tells only those it hadn't. FirstTime sets the expiry, OnceTTL: past the
// outbox's whole retry horizon (2 h 29 min 32 s today) with an hour to spare,
// so a late retry still finds it. TTL is that, rounded up.
var OutboxOnce = withArity(Spec{
	Namespace:   "outbox:once",
	TTL:         4 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategorySweep,
	Description: "One sink having told one person of one outbox event (SET NX); expires past the outbox's retry horizon, which FirstTime sets.",
}, 3) // (event, sink, recipient)

// DraftWriteRate counts one person's draft writes (kept or emptied) in a
// one-minute window (business/Draft): the app saves a draft a second or so
// after typing stops, so a burst past the limit is a client stuck in a loop.
var DraftWriteRate = withArity(Spec{
	Namespace:   "chat:draft:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Draft writes per person. Fixed one-minute window; 120 a minute.",
}, 1) // (userId)

// ThreadWriteRate counts one person's writes to the threads they follow in a
// one-minute window (business/ChatThread): a follow, an unfollow, a thread
// read or marked unread, or all marked read. Opening a thread reads it, so a
// burst past the limit is a client stuck in a loop.
var ThreadWriteRate = withArity(Spec{
	Namespace:   "chat:thread:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Writes to followed threads per person. Fixed one-minute window; 240 a minute.",
}, 1) // (userId)
// WorkGraphRefsCursor is where the nightly reconciler of the work graph's
// references (business/Refs) stopped in one kind's nodes (a graph uid), so
// the next night goes on from there: each night reads a bounded number. It
// is written every night; the expiry only drops a cursor no night has
// renewed in a week, and the next night starts at the beginning.
var WorkGraphRefsCursor = withArity(Spec{
	Namespace:   "workgraph:refs:cursor",
	TTL:         8 * 24 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategorySweep,
	Description: "Where the nightly work-graph references reconciler stopped in one kind's nodes (a graph uid); renewed every night.",
}, 1) // (kind)

// WorkGraphRefsReport is the last nightly reconcile's report (JSON: what it
// read, and the drift it found and fixed, per kind), for the admins' system
// check on whichever instance is asked.
var WorkGraphRefsReport = withArity(Spec{
	Namespace:   "workgraph:refs:report",
	TTL:         8 * 24 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategorySweep,
	Description: "The last nightly work-graph references reconcile's report (JSON), for the system check; renewed every night.",
}, 0)

// DocUpdatedWebhooksDue holds each doc with an open minute of saves, scored by
// when the minute closes (Unix ms): the doc.updated webhook goes out once per
// minute per doc, whichever instance saved it (business/Follow webhooks.go).
// The claimed flush removes each member as its minute closes, so the set
// holds only the last minute's docs; the expiry, renewed on every add, only
// clears what an hour with no instance flushing left behind.
var DocUpdatedWebhooksDue = withArity(Spec{
	Namespace:   "webhooks:doc.updated:due",
	TTL:         time.Hour,
	Datatype:    DatatypeSortedSet,
	Category:    CategoryWebhook,
	Description: "Docs whose minute of saves is open, scored by when it closes; one doc.updated webhook per doc per minute across instances.",
}, 0) // global

// LINK PREVIEWS (docs programme B4) -----------------------------------------

// UnfurlCard caches a link's preview card (business/Unfurl), keyed by the
// SHA-256 of its canonical URL, so a link pasted in many docs is fetched once a
// day. A failed fetch is cached too, for 10 minutes (store's TTL override), so
// an unreachable site isn't asked again on every render.
var UnfurlCard = withArity(Spec{
	Namespace:   "unfurl:card",
	TTL:         24 * time.Hour,
	Datatype:    DatatypeString,
	Category:    CategoryUnfurl,
	Description: "JSON link preview card per canonical URL hash; 24 h, a failure 10 minutes.",
}, 1) // (sha256 of the canonical URL)

// UnfurlRate limits the fetches a person's link previews and stored images
// make: only fetches count, never cache hits.
var UnfurlRate = withArity(Spec{
	Namespace:   "unfurl:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Outbound fetches per (action, user) for link previews and stored remote images. Window-locked; resets every minute.",
}, 2) // (action, userUUID)

// QueryParseRate limits how often one person's query field asks the server
// to read what they're typing (POST /query/parse): a field asks after each
// pause in typing, so the limit sits well above that and stops a loop.
var QueryParseRate = withArity(Spec{
	Namespace:   "query:parse:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Query texts read per user for the search and list fields (POST /query/parse). Window-locked; resets every minute.",
}, 1) // (userUUID)

// PublishPage caches a published doc's rendered page (docs C6, D-25) for a
// short while: every request still resolves its guest grant in Postgres, so
// revoking or unpublishing ends the page at once, and a save, a trash or a
// restore of the doc drops the copy (business/Publish's event sink).
var PublishPage = withArity(Spec{
	Namespace:   "publish:page",
	TTL:         30 * time.Second,
	Datatype:    DatatypeString,
	Category:    CategoryCollab,
	Description: "JSON-encoded rendered page of a doc's published guest link, by grant. Missed when the grant changes; dropped on a save of the doc, and on its trash or restore.",
}, 1) // (grant id)

// PublishPageRate limits how often one published page is worked out afresh
// in a minute, whoever asks: rendered (an answer from PublishPage's copy isn't
// counted, so one host can't keep a page at 429 for everyone), a password
// tried, or a copy made. The per-address limit is PublicReadRate's.
var PublishPageRate = withArity(Spec{
	Namespace:   "publish:page:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Work per doc page outside the workspace (a render, a password tried, a copy made), from anyone. Window-locked; resets every minute.",
}, 1) // (grant id)

// PublishUnlockRate limits password tries at a guest link (a published page
// or a room), per address and link, in a fifteen-minute window.
var PublishUnlockRate = withArity(Spec{
	Namespace:   "publish:unlock:rate",
	TTL:         15 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Password tries per (address, guest link: a page or a room). Window-locked; resets every fifteen minutes.",
}, 2) // (ip, grant id)

// PublishUnlockGrantRate is the ceiling on password tries at one guest link
// (a published page, a room) from every address together, in the same
// fifteen-minute window (business/Guest UnlockTryAllowed).
var PublishUnlockGrantRate = withArity(Spec{
	Namespace:   "publish:unlock:grant:rate",
	TTL:         15 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Password tries per guest link (a page or a room), from every address. Window-locked; resets every fifteen minutes.",
}, 1) // (grant id)

// PublishUnlockSlowRate is an address's tries at a guest link once the link
// is past its ceiling: slower, never shut.
var PublishUnlockSlowRate = withArity(Spec{
	Namespace:   "publish:unlock:slow:rate",
	TTL:         15 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Password tries per (address, guest link) while the link is past its ceiling. Window-locked; resets every fifteen minutes.",
}, 2) // (ip, grant id)

// SearchEntryRepair lets search write a doc's or a board's index entry again
// at most once a window, when a page's live check finds the entry lets in
// someone the graph doesn't (business/GlobalSearch repair.go): a reader
// searching again and again can't make the index rewrite anything more.
var SearchEntryRepair = withArity(Spec{
	Namespace:   "search:entry:repair",
	TTL:         10 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Index entry rewrites search asked for, per (kind, doc or board uuid). Window-locked; resets every ten minutes.",
}, 2) // (kind, uuid)

// QuerySearchRate limits how often one person's search field runs a search
// through the one grammar (POST /query/search): after each pause in typing,
// so the limit sits well above that and stops a loop.
var QuerySearchRate = withArity(Spec{
	Namespace:   "query:search:rate",
	TTL:         1 * time.Minute,
	Datatype:    DatatypeCounter,
	Category:    CategoryRateLimit,
	Description: "Searches run per user through the one query grammar (POST /query/search). Window-locked; resets every minute.",
}, 1) // (userUUID)

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
		PublicReadRate,
		SSOState,
		UserProfile,
		UserDgraphProfile,
		UserSidebar,
		UserChannels,
		UserProjects,
		ChannelBasicInfo,
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
		GuestAccessAudit,
		GuestNotifyOnce,
		InviterDailyInvites,
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
		PasskeyCeremony,
		ConnectorBriefingDay,
		CollabStateSize,
		CallAttendees,
		CallUntranscribed,
		CallTranscriptGrant,
		CallState,
		CallRemoved,
		TranscriptionAgentHeartbeat,
	}
}
