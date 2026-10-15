package business

// Where a call's audio goes while it is transcribed, and whether the server's
// transcriber is there at all.

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// Where the audio goes, as one word the clients turn into a sentence (the
// call's notice and the admin card say it in full).
const (
	// WhereBrowser: each person's browser sends their audio to its speech
	// service: Google in Chrome, Microsoft in Edge.
	WhereBrowser = "browser"
	// WhereServer: the transcriber bundled with OneCamp, on this server. The
	// audio never leaves it.
	WhereServer = "server"
	// WhereCustom: an OpenAI-compatible endpoint the admin typed.
	WhereCustom = "custom"
)

// Where answers where call audio goes in the current configuration, or ""
// when transcription is off.
func Where() string {
	return whereFor(Mode(), STTProvider(), STTBaseURL())
}

// whereFor is Where for a given configuration. Pure.
func whereFor(mode, provider, baseURL string) string {
	switch mode {
	case ModeFrontend:
		return WhereBrowser
	case ModeBackend:
	default:
		return ""
	}
	switch provider {
	case STTLocal:
		return WhereServer
	case STTOpenAI:
		if isOpenAIHost(baseURL) {
			return STTOpenAI
		}
		return WhereCustom
	default:
		return provider
	}
}

// isOpenAIHost reports whether an OpenAI-compatible base URL is OpenAI's own
// (the default when it is empty). Pure.
func isOpenAIHost(baseURL string) bool {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return true
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "api.openai.com")
}

// agentHeartbeat is what the transcription agent last said about itself.
type agentHeartbeat struct {
	At      int64 `json:"at"` // unix ms
	LiveKit bool  `json:"livekit"`
}

// RecordAgentHeartbeat notes that the transcription agent is running, and
// whether it can reach LiveKit (it joins calls through it).
func RecordAgentHeartbeat(ctx context.Context, livekitOK bool) error {
	return redisStore.SetJSON(ctx, registry.TranscriptionAgentHeartbeat, nil, agentHeartbeat{
		At:      time.Now().UnixMilli(),
		LiveKit: livekitOK,
	})
}

// AgentStatus is whether the server's transcriber is running, as the admin
// card shows it.
type AgentStatus struct {
	Running bool  `json:"running"`
	SeenAt  int64 `json:"seen_at"` // unix ms of its last heartbeat, 0 when never
	LiveKit bool  `json:"livekit"` // it can reach the call server
}

// Agent answers whether the transcription agent has said it is running in the
// last 90 seconds (the heartbeat's TTL; it beats every 30).
func Agent(ctx context.Context) AgentStatus {
	var hb agentHeartbeat
	ok, err := redisStore.GetJSON(ctx, registry.TranscriptionAgentHeartbeat, nil, &hb)
	if err != nil || !ok || hb.At == 0 {
		return AgentStatus{}
	}
	return AgentStatus{Running: true, SeenAt: hb.At, LiveKit: hb.LiveKit}
}
