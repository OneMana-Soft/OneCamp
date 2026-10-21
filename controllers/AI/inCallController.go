package controllers

// In-Call AI Agent — a live, in-meeting assistant.
//
// During a video call, any participant can ask the AI a question grounded in
// what's been said so far ("what did we just decide?", "summarize the last 5
// minutes", "what are my action items?"). The answer streams back over SSE,
// exactly like the workspace /ai/ask/stream, but the context is the live call
// transcript instead of workspace RAG.
//
// Transcript context resolution (in priority order):
//  1. recent_transcript from the request body — the FE's in-memory rolling
//     buffer of final utterances, which is the freshest source mid-call
//     (the Dgraph copy only exists while recording AND lags by a write).
//  2. the persisted Dgraph transcript for the room (fallback / when the FE
//     buffer is empty, e.g. a late joiner asking about earlier discussion).
//
// Security: the caller must be a member/participant of the call's surface
// (channel member, DM participant, or group member). The room id shape tells
// us which check to run — same conventions as the recap agent.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	business "github.com/akashc777/OneCamp/business/AI"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	transcriptDomain "github.com/akashc777/OneCamp/domain/LiveKit"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// inCallAskRequest is the body for POST /ai/in-call/ask/stream.
type inCallAskRequest struct {
	// RoomID is the LiveKit room name (channel uuid / "uuidA uuidB" DM /
	// 32-char group hash). Identifies the call surface for permission + context.
	RoomID string `json:"room_id"`
	// Question is the participant's prompt.
	Question string `json:"question"`
	// RecentTranscript is the FE's rolling buffer of recent final utterances,
	// already formatted as "Name: text" lines (newest last). Optional; when
	// empty we fall back to the persisted transcript.
	RecentTranscript string `json:"recent_transcript"`
	// QuestionID is the asker's id for the question, which everyone's panel
	// shows. When given, the server sends the finished answer to the whole
	// call itself (inCallAnswerPacket).
	QuestionID string `json:"question_id"`
}

// questionID is the shape of a question's id: the client's own, short.
var questionID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// inCallAnswerTopic is the call's data topic for the in-call assistant.
const inCallAnswerTopic = "oc.ai"

// inCallAnswerMaxBytes keeps an answer inside one reliable data packet.
const inCallAnswerMaxBytes = 12 * 1024

// inCallAnswerPacket is the finished answer to a question, as the server sends
// it to everyone in the call.
//
// Answers used to be sent by the asker's browser and accepted from anyone for
// any question id, so any participant could replace the assistant's answer
// to someone else's question in everybody's panel, and the asker could put any
// words in the assistant's mouth. Sent by the server, with no sending
// participant and the asker's identity in it, an answer can only be the
// assistant's, to the question that person asked. Pure.
func inCallAnswerPacket(id, answer, errMsg, askedBy string) []byte {
	if len(answer) > inCallAnswerMaxBytes {
		answer = strings.ToValidUTF8(answer[:inCallAnswerMaxBytes], "")
	}
	b, _ := json.Marshal(struct {
		V         int    `json:"v"`
		T         string `json:"t"`
		ID        string `json:"id"`
		Answer    string `json:"answer"`
		Error     string `json:"error,omitempty"`
		AskedByID string `json:"askedById"`
	}{1, "a", id, answer, errMsg, askedBy})
	return b
}

// broadcastInCallAnswer sends the finished answer to everyone in the call, on
// behalf of an asker who is in it. Best-effort: the asker has it already.
func broadcastInCallAnswer(ctx context.Context, req inCallAskRequest, asker, answer, errMsg string) {
	if req.QuestionID == "" {
		return
	}
	if !livekitInit.LiveKitService.Ready() {
		return
	}
	// Only into a call the asker is in.
	present, err := livekitInit.LiveKitService.RoomHasParticipant(ctx, req.RoomID, asker)
	if err != nil || !present {
		return
	}
	if err := livekitInit.LiveKitService.SendData(ctx, req.RoomID, inCallAnswerTopic,
		inCallAnswerPacket(req.QuestionID, answer, errMsg, asker)); err != nil {
		helpers.LogInfoWithContext(ctx, "controllers/AI in-call answer not broadcast: %v", err)
	}
}

const (
	inCallMaxQuestion   = 2000
	inCallMaxTranscript = 24000 // bound prompt size
	inCallFallbackLines = 400   // persisted-transcript fallback cap
)

// inCallSystemPrompt keeps answers tight, grounded, and call-appropriate.
//
// It distinguishes two request shapes, because conflating them produced bad
// UX (a "summarize" request getting a "that hasn't come up yet" refusal even
// when intros HAD happened):
//   - SUMMARY / RECAP / STATUS ("summarize…", "what's going on", "what did we
//     decide", "action items"): always describe whatever IS in the transcript,
//     even if it's just introductions. Only say it's early if the transcript
//     is genuinely empty.
//   - SPECIFIC factual question ("what's the budget?"): answer from the
//     transcript; if that specific fact isn't present, say so briefly.
const inCallSystemPrompt = `You are OneCamp's in-call assistant. You are given the recent transcript of a live voice/video call, then a question from a participant. Ground every answer in the transcript only — never invent facts, names, decisions, or numbers that aren't there.

Decide what the participant is asking:

1) SUMMARY / RECAP / STATUS request (e.g. "summarize…", "what's going on", "catch me up", "what did we decide", "what are my action items", "recap"):
   - Summarize what has ACTUALLY been said so far, even if it's only introductions or small talk. Be honest about how little or how much there is.
   - If people have only introduced themselves, say exactly that (e.g. "So far it's just introductions — Akash joined and said hello.").
   - For "what did we decide" / "action items", use a short bullet list; if none yet, say "No decisions yet." / "No action items yet."
   - Only say the call hasn't started if the transcript is truly empty.

2) SPECIFIC factual question (asks about a particular detail):
   - Answer directly from the transcript. If that specific detail genuinely hasn't been mentioned, say so in one short sentence — and do NOT also dump unrelated content.

Style: concise and direct (this is a live meeting, not an essay). No preamble, no markdown headers, under ~120 words. Never contradict yourself by saying nothing has come up and then describing what came up.`

// InCallAskStream handles POST /ai/in-call/ask/stream — SSE, transcript-grounded.
func InCallAskStream(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req inCallAskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}
	req.RoomID = strings.TrimSpace(req.RoomID)
	req.Question = strings.TrimSpace(req.Question)
	if req.RoomID == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say which call."})
		return
	}
	if req.Question == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Ask a question first."})
		return
	}
	if len(req.Question) > inCallMaxQuestion {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That question is too long."})
		return
	}
	req.QuestionID = strings.TrimSpace(req.QuestionID)
	if req.QuestionID != "" && !questionID.MatchString(req.QuestionID) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That question can't be answered: its id isn't valid."})
		return
	}
	asker := userInfo.UserDgraphInfo.Uid

	// Permission: the caller must belong to the call's surface.
	if !callerCanAccessRoom(ctx, &userInfo, req.RoomID) {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "You're not in this call."})
		return
	}

	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "AI is not enabled"})
		return
	}
	if err := svc.Resiliency.PreCheck(ctx, userInfo.UserDgraphInfo.Uuid); err != nil {
		code := http.StatusTooManyRequests
		if err == ai.ErrCircuitOpen {
			code = http.StatusServiceUnavailable
		}
		helpers.WriteJSON(w, code, helpers.Envolope{"msg": err.Error()})
		return
	}

	// Resolve transcript context: prefer the FE's live buffer, fall back to
	// the persisted Dgraph transcript for late joiners / empty buffers.
	transcript := strings.TrimSpace(req.RecentTranscript)
	if transcript == "" {
		transcript = fetchPersistedTranscript(ctx, req.RoomID)
	}
	transcript = clampTranscriptTail(transcript, inCallMaxTranscript)

	// SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Streaming not supported"})
		return
	}

	var userContent string
	if transcript != "" {
		userContent = "Live call transcript so far (each line is \"Speaker: text\"):\n" +
			transcript + "\n\nParticipant's question: " + req.Question
	} else {
		userContent = "The live call transcript is EMPTY so far (no one has spoken yet, or transcription/captions are off).\n\nParticipant's question: " + req.Question
	}

	messages := []ai.ChatMessage{
		{Role: "system", Content: inCallSystemPrompt},
		{Role: "user", Content: userContent},
	}

	if err := svc.Resiliency.CB.Allow(); err != nil {
		writeSSEError(w, flusher, "AI is busy, try again in a moment")
		broadcastInCallAnswer(ctx, req, asker, "", "AI is busy, try again in a moment")
		return
	}
	stream, err := svc.StreamChat(ctx, messages, ai.ChatOptions{Temperature: 0.3, MaxTokens: 600})
	if err != nil {
		svc.Resiliency.CB.RecordResult(err)
		writeSSEError(w, flusher, err.Error())
		broadcastInCallAnswer(ctx, req, asker, "", "The answer couldn't be generated.")
		return
	}

	var acc strings.Builder
	for chunk := range stream {
		if chunk.Error != nil {
			svc.Resiliency.CB.RecordResult(chunk.Error)
			writeSSEError(w, flusher, chunk.Error.Error())
			broadcastInCallAnswer(ctx, req, asker, "", "The answer couldn't be generated.")
			return
		}
		if chunk.Content != "" {
			acc.WriteString(chunk.Content)
			// json.Marshal handles ALL escaping (backslash, CR, tab, control
			// chars, unicode) — the prior hand-rolled \n/" replace dropped
			// those and produced frames the FE silently discarded.
			if b, mErr := json.Marshal(map[string]string{"content": chunk.Content}); mErr == nil {
				fmt.Fprintf(w, "data: %s\n\n", string(b))
				flusher.Flush()
			}
		}
		if chunk.Done {
			break
		}
	}

	// Sanitize (drop any tool/UUID leakage from small models) and, if it
	// changed, tell the FE to swap the displayed text.
	sanitized := business.SanitizeResponse(acc.String())
	// If the model returned nothing usable (empty stream, or sanitize removed
	// everything), send a graceful fallback so the bubble never hangs blank.
	if strings.TrimSpace(sanitized) == "" {
		sanitized = "I couldn't generate an answer for that. Try rephrasing, or ask once more people have spoken."
		if b, mErr := json.Marshal(map[string]string{"replace": sanitized}); mErr == nil {
			fmt.Fprintf(w, "data: %s\n\n", string(b))
			flusher.Flush()
		}
	} else if sanitized != acc.String() {
		if b, mErr := json.Marshal(map[string]string{"replace": sanitized}); mErr == nil {
			fmt.Fprintf(w, "data: %s\n\n", string(b))
			flusher.Flush()
		}
	}

	fmt.Fprintf(w, "data: {\"done\": true}\n\n")
	flusher.Flush()
	svc.Resiliency.CB.RecordSuccess()
	broadcastInCallAnswer(ctx, req, asker, sanitized, "")
}

// writeSSEError emits a single SSE error frame.
func writeSSEError(w http.ResponseWriter, flusher http.Flusher, msg string) {
	writeSSE(w, map[string]string{"error": msg})
	flusher.Flush()
}

// clampTranscriptTail bounds the transcript to maxBytes, keeping the TAIL (the
// most recent discussion is the most relevant) WITHOUT cutting a line in half —
// it trims forward to the next newline so the model never sees a half-line.
func clampTranscriptTail(transcript string, maxBytes int) string {
	if len(transcript) <= maxBytes {
		return transcript
	}
	tail := transcript[len(transcript)-maxBytes:]
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:]
	}
	return strings.TrimSpace(tail)
}

// fetchPersistedTranscript pulls the room's persisted transcript from Dgraph
// and renders it as "Name: text" lines (matching the FE live-buffer format the
// prompt expects). Speaker uids are resolved to display names, cached per call.
// Best-effort: returns "" on any error or when nothing is recorded.
func fetchPersistedTranscript(ctx context.Context, roomID string) string {
	lines, _, err := transcriptDomain.GetTranscriptLinesByRoom(ctx, roomID, inCallFallbackLines)
	if err != nil || len(lines) == 0 {
		return ""
	}

	// Every speaker's name in one read (backend audit #22).
	var uids []string
	for _, l := range lines {
		if l.ParticipantIdentity != "" {
			uids = append(uids, l.ParticipantIdentity)
		}
	}
	nameByUID, _ := userDomain.UserNamesByDgraphUIDs(ctx, uids)
	var sb strings.Builder
	for _, l := range lines {
		t := strings.TrimSpace(l.Text)
		if t == "" {
			continue
		}
		name := "participant"
		if n := nameByUID[l.ParticipantIdentity]; n != "" {
			name = n
		}
		sb.WriteString(name)
		sb.WriteString(": ")
		sb.WriteString(t)
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

// callerCanAccessRoom verifies the user belongs to the call surface identified
// by roomID, using the same room-name shape conventions as the recap agent:
//   - channel call → UUID with hyphens → must be a channel member/admin
//   - DM call       → "uuidA uuidB"     → caller must be one of the two
//   - group chat    → 32-char hex hash  → caller must be a group member
func callerCanAccessRoom(ctx context.Context, userInfo *userModels.UserInfo, roomID string) bool {
	switch {
	case strings.HasPrefix(roomID, "meet-"):
		// An instant meeting: its host, the one member in it (everyone else
		// is a guest, who has no session). It was parsed as a channel uuid,
		// which failed, so the host's every question answered 403.
		ok, err := liveKitBusiness.MemberMayBeInCall(ctx, roomID, userInfo.UserDgraphInfo.Uid)
		return err == nil && ok

	case strings.Contains(roomID, " "):
		// DM: caller must be one of the participant UUIDs in the grouping id.
		self := userInfo.UserDgraphInfo.Uuid
		for _, p := range strings.Fields(roomID) {
			if p == self {
				return true
			}
		}
		return false

	case !strings.Contains(roomID, "-") && len(roomID) == 32:
		// Group chat: membership check via the chat business layer.
		dm, err := chatBusiness.GetDgraphDmBasicInfoFromDgraph(ctx, userInfo.UserDgraphInfo.Uid, roomID)
		return err == nil && dm != nil && dm.ParticipantIsMember == 1

	default:
		// Channel: must be a member or admin.
		if _, perr := uuid.Parse(roomID); perr != nil {
			return false
		}
		ch, err := channelDomain.GetDgraphChannelInfoByUUID(ctx, roomID, userInfo.UserDgraphInfo.Uid)
		if err != nil || ch == nil {
			return false
		}
		return ch.IsMember == 1 || ch.IsAdmin == 1
	}
}
