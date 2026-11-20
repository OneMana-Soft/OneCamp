package business

// Where a call's audio goes while it is transcribed, and whether the server's
// transcriber is there at all.

import (
	"context"
	"net/url"
	"os"
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

// LeavesServer reports whether call audio sent to where leaves this server:
// everywhere but the bundled transcriber ("" is nowhere: transcription off).
func LeavesServer(where string) bool {
	return where != "" && where != WhereServer
}

// Why captions can't run, as a stable code the clients put in words (the
// words, and what an admin should do, are the web's).
const (
	// ReasonTranscriptionDisabled: an admin turned transcription off.
	ReasonTranscriptionDisabled = "transcription_disabled"
	// ReasonTranscriberNotRunning: server mode, and the server's transcriber
	// (the agent that joins calls, started by `make stt_up` with the bundled
	// model) hasn't said it is running, or it can't reach the call server.
	// Captions stay off; nothing falls through to a browser or a cloud.
	ReasonTranscriberNotRunning = "transcriber_not_running"
)

// CaptionsOffReason is why captions can't run now, "" when they can. The
// transcriber's heartbeat is read only in server mode, which alone needs it.
func CaptionsOffReason(ctx context.Context) string {
	mode := Mode()
	if mode != ModeBackend {
		return captionsOffReason(mode, AgentStatus{})
	}
	return captionsOffReason(mode, Agent(ctx))
}

// captionsOffReason is CaptionsOffReason for a mode and the agent's status.
// Pure.
func captionsOffReason(mode string, agent AgentStatus) string {
	switch {
	case mode == ModeOff:
		return ReasonTranscriptionDisabled
	case mode == ModeBackend && !(agent.Running && agent.LiveKit):
		return ReasonTranscriberNotRunning
	default:
		return ""
	}
}

// AcknowledgedWhere is where an admin last acknowledged call audio going, ""
// when nobody has.
func AcknowledgedWhere() string {
	return strings.TrimSpace(loadAll()[keyAudioAck])
}

// AcknowledgementRequired refuses a save that would send call audio off the
// server, to Where, without the admin's acknowledgement of exactly that place.
type AcknowledgementRequired struct{ Where string }

func (e *AcknowledgementRequired) Error() string {
	return "call audio would go to " + e.Where + ": acknowledge it first"
}

// Acknowledgement is an admin's new acknowledgement of where call audio goes,
// for its audit row: the place, and the configuration that sends it there.
type Acknowledgement struct {
	Where    string
	Mode     string
	Provider string // the speech provider, server mode only
	Host     string // the endpoint's host, for an address the admin typed
}

// Acknowledge checks a save against where it would send call audio and the
// acknowledgement given with it (a Where word, "" for none). It returns the
// acknowledgement to record when this one is new, nil when there is nothing
// to record, or *AcknowledgementRequired. The caller records it in the audit
// log, then saves it (SaveInput.Acknowledged).
func Acknowledge(in SaveInput, given string) (*Acknowledgement, error) {
	mode, provider, baseURL := configAfter(in)
	where := whereFor(mode, provider, baseURL)
	record, err := checkAcknowledgement(where, AcknowledgedWhere(), strings.TrimSpace(given))
	if err != nil || !record {
		return nil, err
	}
	a := &Acknowledgement{Where: where, Mode: mode}
	if mode == ModeBackend {
		a.Provider = provider
	}
	if where == WhereCustom {
		if u, err := url.Parse(baseURL); err == nil {
			a.Host = u.Hostname()
		}
	}
	return a, nil
}

// checkAcknowledgement decides a save to next, over the stored
// acknowledgement, with the one given: audio staying on the server needs none;
// leaving it needs the stored one or a given one naming exactly next, and only
// a given one that isn't stored yet is recorded. Pure.
func checkAcknowledgement(next, stored, given string) (bool, error) {
	if !acknowledgementNeeded(next, stored) {
		return false, nil
	}
	if given != next {
		return false, &AcknowledgementRequired{Where: next}
	}
	return true, nil
}

// acknowledgementNeeded reports whether audio going to where needs an
// acknowledgement the admin hasn't given. Pure.
func acknowledgementNeeded(where, stored string) bool {
	return LeavesServer(where) && stored != where
}

// configAfter is the mode, provider and address once in is saved: each
// field in sets, else what is set now, read the way Mode, STTProvider and
// STTBaseURL read it.
func configAfter(in SaveInput) (mode, provider, baseURL string) {
	mode, provider, baseURL = Mode(), STTProvider(), STTBaseURL()
	if in.Mode != nil {
		mode = normalizeMode(*in.Mode)
	}
	if in.STTProvider != nil {
		provider = normalizeSTT(*in.STTProvider)
	}
	if in.STTBaseURL != nil {
		if baseURL = strings.TrimSpace(*in.STTBaseURL); baseURL == "" {
			baseURL = strings.TrimSpace(os.Getenv("STT_BASE_URL"))
		}
	}
	return mode, provider, baseURL
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
