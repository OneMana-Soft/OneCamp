package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	business "github.com/akashc777/OneCamp/business/AI"
	chatSessionBusiness "github.com/akashc777/OneCamp/business/AIChatSession"
	codeagent "github.com/akashc777/OneCamp/business/CodeAgent"
	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	transcriptionBusiness "github.com/akashc777/OneCamp/business/Transcription"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// CatchUp handles POST /ai/catch-up
// Generates an AI recap of what the user missed in a scope since they last
// looked. scope_type ∈ channel | chat | workspace.
func CatchUp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.CatchUpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse body",
			"err": err.Error(),
		})
		return
	}

	req.ScopeType = strings.TrimSpace(req.ScopeType)
	if req.ScopeType == "" {
		req.ScopeType = "workspace"
	}

	// Validate scope identifiers BEFORE they reach the OpenSearch term
	// clause (defense against query injection, mirroring the summarize
	// handlers).
	switch req.ScopeType {
	case "channel":
		req.ChannelUUID = strings.TrimSpace(req.ChannelUUID)
		if _, err := uuid.Parse(req.ChannelUUID); err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "channel_uuid must be a UUID"})
			return
		}
	case "chat":
		req.ChatGrpID = strings.TrimSpace(req.ChatGrpID)
		req.ToUserUUID = strings.TrimSpace(req.ToUserUUID)
		// Either a DM peer UUID or an explicit group grouping id is required.
		if req.ToUserUUID != "" {
			if _, err := uuid.Parse(req.ToUserUUID); err != nil {
				helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "to_user_uuid must be a UUID"})
				return
			}
		} else if !dgraphquery.IsAllowedFilterValue(req.ChatGrpID) {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "chat_grp_id has unsupported characters"})
			return
		}
	case "workspace":
		// no scope id
	default:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "scope_type must be channel, chat, or workspace"})
		return
	}

	resp, err := business.GetCatchUp(ctx, &userInfo, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/CatchUp Failed err: %+v", err)
		statusCode := http.StatusInternalServerError
		if err == ai.ErrRateLimited {
			statusCode = http.StatusTooManyRequests
		} else if err == ai.ErrCircuitOpen {
			statusCode = http.StatusServiceUnavailable
		}
		helpers.WriteJSON(w, statusCode, helpers.Envolope{"msg": "Failed to build catch-up", "err": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Catch-up ready", "data": resp})
}

// SummarizeChannel handles POST /ai/summarize/channel
// Generates an AI summary of recent messages in a channel.
func SummarizeChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.SummarizeChannelRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/SummarizeChannel Failed to parse the body of the req err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse body",
			"err": err.Error(),
		})
		return
	}

	req.ChannelUUID = strings.TrimSpace(req.ChannelUUID)
	if len(req.ChannelUUID) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "channel_uuid is required",
		})
		return
	}
	// Strict UUID validation. Without it the value is interpolated
	// into an OpenSearch JSON term clause via fmt.Sprintf in
	// services/AI/embeddings.go::SearchRecent — a payload containing
	// `"` would inject extra clauses.
	if _, err := uuid.Parse(req.ChannelUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "channel_uuid must be a UUID",
		})
		return
	}

	// Extract localization context
	localization := map[string]string{
		"timezone":   req.Timezone,
		"location":   req.Location,
		"local_time": req.LocalTime,
	}

	summary, err := business.SummarizeChannel(ctx, &userInfo, req.ChannelUUID, req.MessageCount, localization)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/SummarizeChannel Failed to summarize channel err: %+v",
			err)

		statusCode := http.StatusInternalServerError
		if err == ai.ErrRateLimited {
			statusCode = http.StatusTooManyRequests
		} else if err == ai.ErrCircuitOpen {
			statusCode = http.StatusServiceUnavailable
		}

		helpers.WriteJSON(w, statusCode, helpers.Envolope{
			"msg": "Failed to summarize channel",
			"err": err.Error(),
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "Channel summarized successfully!",
		"data": summary,
	})
}

// SummarizeDM handles POST /ai/summarize/dm
// Generates an AI summary of recent messages in a 1:1 chat.
func SummarizeDM(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.SummarizeDMRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse body",
			"err": err.Error(),
		})
		return
	}

	req.ToUserUUID = strings.TrimSpace(req.ToUserUUID)
	if len(req.ToUserUUID) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "to_user_uuid is required",
		})
		return
	}
	if _, err := uuid.Parse(req.ToUserUUID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "to_user_uuid must be a UUID",
		})
		return
	}

	localization := map[string]string{
		"timezone":   req.Timezone,
		"location":   req.Location,
		"local_time": req.LocalTime,
	}

	summary, err := business.SummarizeDM(ctx, &userInfo, req.ToUserUUID, req.MessageCount, localization)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SummarizeDM Failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to summarize DM", "err": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "DM summarized successfully!", "data": summary})
}

// SummarizeGroup handles POST /ai/summarize/group
// Generates an AI summary of recent messages in a group chat.
func SummarizeGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.SummarizeGroupRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse body",
			"err": err.Error(),
		})
		return
	}

	req.ChatGrpID = strings.TrimSpace(req.ChatGrpID)
	if len(req.ChatGrpID) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "chat_grp_id is required",
		})
		return
	}
	// chat_grp_id is a deterministic hash of participant UUIDs; in
	// practice it's always a hex-like string. Reject anything that
	// could break out of the OpenSearch JSON term clause.
	if !dgraphquery.IsAllowedFilterValue(req.ChatGrpID) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "chat_grp_id has unsupported characters",
		})
		return
	}

	localization := map[string]string{
		"timezone":   req.Timezone,
		"location":   req.Location,
		"local_time": req.LocalTime,
	}

	summary, err := business.SummarizeGroupChat(ctx, &userInfo, req.ChatGrpID, req.MessageCount, localization)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SummarizeGroup Failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to summarize group chat", "err": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Group chat summarized successfully!", "data": summary})
}

// AskAI handles POST /ai/ask
// Answers a user's question using workspace context.
func AskAI(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.AskAIRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AskAI Failed to parse the body of the req err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse body",
			"err": err.Error(),
		})
		return
	}

	req.Question = strings.TrimSpace(req.Question)
	if len(req.Question) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "question is required",
		})
		return
	}
	if len(req.Question) > 5000 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "question is too long (max 5000 characters)",
		})
		return
	}

	// Extract localization context
	localization := map[string]string{
		"timezone":   req.Timezone,
		"location":   req.Location,
		"local_time": req.LocalTime,
	}

	answer, err := business.AskAI(ctx, &userInfo, req.Question, req.SessionID, localization)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/AskAI Failed to get AI answer err: %+v",
			err)

		statusCode := http.StatusInternalServerError
		if err == ai.ErrRateLimited {
			statusCode = http.StatusTooManyRequests
		} else if err == ai.ErrCircuitOpen {
			statusCode = http.StatusServiceUnavailable
		}

		helpers.WriteJSON(w, statusCode, helpers.Envolope{
			"msg": "Failed to get AI answer",
			"err": err.Error(),
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "AI answer generated successfully!",
		"data": answer,
	})
}

// ExecuteAction handles POST /ai/action/execute
// Executes a confirmed workspace action (create task, create doc, etc.).
func ExecuteAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.ExecuteActionRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/ExecuteAction Failed to parse the body of the req err: %+v",
			err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse body",
			"err": err.Error(),
		})
		return
	}

	req.ToolName = strings.TrimSpace(req.ToolName)
	if len(req.ToolName) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "tool_name is required",
		})
		return
	}

	// Ensure Params is never nil (client may send null or omit it)
	if req.Params == nil {
		req.Params = make(map[string]string)
	}

	// Extract localization context
	localization := map[string]string{
		"timezone":   req.Timezone,
		"location":   req.Location,
		"local_time": req.LocalTime,
	}

	result, err := business.ExecuteAction(ctx, &userInfo, req.ToolName, req.Params, localization)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"controllers/ExecuteAction Failed to execute action err: %+v",
			err)

		statusCode := http.StatusInternalServerError
		if err == ai.ErrRateLimited {
			statusCode = http.StatusTooManyRequests
		} else if err == ai.ErrCircuitOpen {
			statusCode = http.StatusServiceUnavailable
		}

		helpers.WriteJSON(w, statusCode, helpers.Envolope{
			"msg": "Failed to execute action",
			"err": err.Error(),
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "Action executed successfully!",
		"data": result,
	})
}

// AnalyzeImage handles POST /ai/analyze-image. Describes an image attachment
// the user can access using the configured vision model. The FE supplies the
// attachment's obj_uuid + src_key/src_value (from the rendered message); the
// user never provides an id.
func AnalyzeImage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.AnalyzeImageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}
	if len(req.Prompt) > 2000 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "prompt is too long"})
		return
	}

	description, err := business.AnalyzeAttachmentImage(ctx, &userInfo, req.SrcKey, req.SrcRef, req.ObjUuid, req.Prompt)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"data":   map[string]string{"description": description},
	})
}

// AnalyzeDocument handles POST /ai/analyze-document. Reads a document
// attachment the user can access (DOCX / text-family) and returns a summary or
// an answer to the supplied prompt. Same identifier + access model as
// AnalyzeImage; the FE supplies obj_uuid + src_key/src_ref from the rendered
// message.
func AnalyzeDocument(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.AnalyzeImageRequest // same shape: obj_uuid + src_key/src_ref + prompt
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}
	if len(req.Prompt) > 2000 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "prompt is too long"})
		return
	}

	answer, err := business.AnalyzeAttachmentDocument(ctx, &userInfo, req.SrcKey, req.SrcRef, req.ObjUuid, req.Prompt)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"data":   map[string]string{"description": answer},
	})
}

// Translate handles POST /ai/translate. Translates the supplied text into the
// requested target language (name or BCP-47 code; blank = English) using the
// member's model. Self-scoped; the business layer enforces limits and hardens
// against prompt injection.
func Translate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req struct {
		Text           string `json:"text"`
		TargetLanguage string `json:"target_language"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}

	translated, err := business.TranslateText(ctx, &userInfo, req.Text, req.TargetLanguage)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"data":   map[string]string{"translation": translated},
	})
}

// maxDictationAudioBytes caps a dictation clip. Voice input is short (seconds),
// so a small cap is plenty and protects the server + upstream STT.
const maxDictationAudioBytes = 15 << 20 // 15 MB

// VoiceInputStatus handles GET /ai/voice-input — reports whether server-side
// voice dictation is available (an OpenAI-compatible or Deepgram STT is
// configured), so the FE only shows the mic when it will work.
func VoiceInputStatus(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"data":   map[string]bool{"available": transcriptionBusiness.DictationAvailable()},
	})
}

// Transcribe handles POST /ai/transcribe — voice dictation: accepts a short
// recorded audio clip (multipart "audio") and returns its transcript via the
// admin-managed, model-agnostic STT (reused from call transcription). Member-
// accessible; rate-limited per user; size-capped.
func Transcribe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	if !transcriptionBusiness.DictationAvailable() {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "voice input isn't configured for this workspace"})
		return
	}

	// Reuse the per-user AI rate limiter to prevent abuse (STT is a served
	// call). Best-effort: skip when the AI service isn't initialized.
	if svc := ai.GetService(); svc != nil && svc.Resiliency != nil {
		if err := svc.Resiliency.CheckRateLimit(ctx, userInfo.UserDgraphInfo.Uuid); err != nil {
			helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": err.Error()})
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxDictationAudioBytes)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "audio is too large or malformed"})
		return
	}
	file, hdr, err := r.FormFile("audio")
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "missing audio"})
		return
	}
	defer file.Close()

	audio, err := io.ReadAll(io.LimitReader(file, maxDictationAudioBytes+1))
	if err != nil || len(audio) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "could not read audio"})
		return
	}
	if len(audio) > maxDictationAudioBytes {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "audio clip is too long"})
		return
	}

	filename := "audio.webm"
	contentType := "audio/webm"
	if hdr != nil {
		if hdr.Filename != "" {
			filename = hdr.Filename
		}
		if ct := hdr.Header.Get("Content-Type"); ct != "" {
			contentType = ct
		}
	}

	text, terr := transcriptionBusiness.TranscribeAudio(ctx, audio, filename, contentType)
	if terr != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": terr.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"data":   map[string]string{"text": text},
	})
}

// AskAIStream handles POST /ai/ask/stream
// Streams an AI answer using Server-Sent Events (SSE).
func AskAIStream(w http.ResponseWriter, r *http.Request) {
	// Enforce a streaming deadline to prevent zombie SSE connections. Tunable
	// (AI_STREAM_TIMEOUT_SECONDS) so slow local models get enough first-token
	// headroom.
	ctx, cancel := context.WithTimeout(r.Context(), ai.StreamTimeout())
	defer cancel()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.AskAIRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse body",
			"err": err.Error(),
		})
		return
	}

	req.Question = strings.TrimSpace(req.Question)
	if len(req.Question) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "question is required",
		})
		return
	}
	if len(req.Question) > 5000 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "question is too long (max 5000 characters)",
		})
		return
	}

	// Check resiliency before streaming
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{
			"msg": "AI service is not enabled",
		})
		return
	}

	// Resolve this user's model (their admin-authorized pick, else the
	// workspace default) and its dedicated circuit breaker. Rate limiting is
	// model-independent and stays on the shared ResiliencyManager.
	// WithLimits so the prompt budget and the overflow rescue both measure against the
	// model this member actually picked, not the workspace default.
	llm, cb, limits := svc.ResolveUserModelWithLimits(ctx, userInfo.UserDgraphInfo.Uuid)
	ctx = ai.WithModelLimits(ctx, limits)
	// Records any shortening of the prompt so the answer can say it happened, rather
	// than quietly returning a reply built from half the thread.
	ctx = ai.WithContextNoticeSink(ctx)
	// What was asked, so a connector that is down is mentioned when it would
	// have mattered to this question and left out when it would not.
	ai.NoteQuestion(ctx, req.Question)
	if err := cb.Allow(); err != nil {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": err.Error()})
		return
	}
	if err := svc.Resiliency.CheckRateLimit(ctx, userInfo.UserDgraphInfo.Uuid); err != nil {
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": err.Error()})
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering

	flusher, ok := w.(http.Flusher)
	if !ok {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Streaming not supported",
		})
		return
	}

	// Fast-path: skip expensive context building for conversational inputs.
	// BuildUserContextPublic generates an embedding + k-NN search (~3-5s),
	// which is unnecessary for greetings like "hi", "hello", etc.
	var contextText string
	var contextSources []adapter.SourceRef
	// Localization is needed both for context building and for any tools the
	// agent loop executes, so it lives at handler scope.
	localization := map[string]string{
		"timezone":   req.Timezone,
		"location":   req.Location,
		"local_time": req.LocalTime,
	}
	isConversational := ai.IsConversational(req.Question)
	if !isConversational {
		var ctxErr error
		contextText, contextSources, ctxErr = business.BuildUserContextPublic(ctx, &userInfo, req.Question, localization)
		if ctxErr != nil {
			contextText = ""
			contextSources = nil
		}
		// Hard token ceiling on the assembled context so the total prompt
		// stays within the model's window (mirrors the non-streaming AskAI).
		contextText = limits.TruncateForPrompt(ctx, contextText, limits.ContextBudget())
	}

	// The conversation this exchange belongs to.
	//
	// THE BUG THIS FIXES. Nothing on the streaming path ever created a session.
	// The controller read req.SessionID and used it when set, the client started
	// at null and had no way to learn one, and no code anywhere called
	// CreateSession for a stream. So the id was ALWAYS empty here, every branch
	// guarded by it was dead, and the assistant has never had multi-turn memory:
	// each question was answered with no knowledge of the last.
	//
	// Minted here rather than by CreateSession because that returns an empty
	// string when Redis is down, and the durable record in Postgres does not
	// depend on Redis being up. One id, both stores.
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sessionID = uuid.New().String()
	}

	// Load session history if provided
	var history []ai.ChatMessage
	if sessionID != "" {
		history, _ = ai.GetSessionHistory(ctx, sessionID, userInfo.UserDgraphInfo.Uuid)
		// Redis holds the working history for half an hour. A person who comes
		// back to a conversation after lunch, or after a restart, sees their
		// messages on screen because those come from Postgres, and would have
		// been answered as if the exchange never happened. Fall back to the
		// durable copy so what the model is told matches what is displayed.
		if len(history) == 0 {
			history = durableHistory(ctx, sessionID, userInfo.UserPostgresInfo.Id)
		}
		// Bound history by a token budget so it can't push the prompt past
		// the context window (mirrors the non-streaming AskAI path).
		history = limits.TrimHistoryForPrompt(ctx, history, limits.HistoryBudget())
	}

	// Select appropriate strategy: factual RAG vs. creative conversation
	var systemPrompt string
	opts := ai.ChatOptions{
		MaxTokens:  1024,
		Seed:       int(time.Now().UnixNano() % 1000000), // Inject variety for identical prompts
		LowLatency: true,                                 // live chat: fail fast on rate limits
	}

	// Determine which external connectors this user has linked, so the prompt
	// makes the model aware of what it can actually access.
	connected := connectorBusiness.ConnectedProviderNames(ctx, userInfo.UserDgraphInfo.Uuid)

	if isConversational {
		// Creative/Casual: higher temperature for variety
		systemPrompt = business.AskAIConversationalPromptWithConnectors(false, connected)
		opts.Temperature = 0.8
		opts.TopP = 0.9
	} else {
		// Factual RAG: lower temperature for deterministic sources
		// Skip tool definitions for read-only/summary questions to save ~500
		// tokens — UNLESS a write verb co-occurs ("summarize X and DM Y"), in
		// which case the agent loop needs the tools to perform the write step.
		includeTools := ai.ShouldIncludeTools(req.Question)
		systemPrompt = business.AskAISystemPromptWithConnectorsForQuery(includeTools, connected, req.Question)
		opts.Temperature = 0.3
	}

	messages := []ai.ChatMessage{
		{Role: "system", Content: systemPrompt},
	}

	// Inject conversation history
	if len(history) > 0 {
		messages = append(messages, history...)
	}

	// Build user message with context
	userContent := req.Question
	if contextText != "" {
		userContent = fmt.Sprintf("Context from workspace:\n%s\n\nQuestion: %s", contextText, req.Question)
	}
	messages = append(messages, ai.ChatMessage{Role: "user", Content: userContent})

	// Sent first, before any content. The client adopts it so the NEXT question
	// continues this conversation, and so a reader can find it in the history
	// list even if the connection drops before the answer finishes.
	fmt.Fprintf(w, "data: {\"session_id\": %q}\n\n", sessionID)
	flusher.Flush()

	// From here the answer belongs to the server, not to the connection. A tab
	// that closes or a network that drops no longer cancels the model call and
	// loses what it wrote; the answer finishes, is saved to the session, and is
	// there when the person comes back. Only a deliberate stop from the owner
	// (POST /ai/ask/stop) ends it early, and even then the partial answer is
	// kept. See services/AI/streamLifetime.go for the whole argument.
	lt := ai.NewStreamLifetime(ctx, sessionID, userInfo.UserDgraphInfo.Uuid, ai.StreamTimeout())
	defer lt.Close()
	// Writes to a client that has gone are wasted work, not errors.
	send := func(frame string) {
		if lt.ClientGone() {
			return
		}
		fmt.Fprint(w, frame)
		flusher.Flush()
	}

	stream, err := ai.ChatStreamWithRescue(lt.Gen, llm, messages, opts)
	if err != nil {
		cb.RecordResult(err)
		errJSON, _ := json.Marshal(ai.FriendlyProviderError(err))
		send(fmt.Sprintf("data: {\"error\": %s}\n\n", string(errJSON)))
		return
	}

	// Stream chunks to client in real-time for smooth UX,
	// while accumulating for post-stream tool call parsing.
	var accumulated strings.Builder
	// Suppress reasoning-model chain-of-thought (<think>...</think>) from the
	// live view; the full text is still accumulated for tool parsing and the
	// final sanitized "replace" event.
	thinkFilter := &business.StreamThinkFilter{}

	for chunk := range stream {
		if chunk.Error != nil {
			if lt.Stopped() {
				// The person pressed stop. What the model said before that is
				// theirs and is saved below, not discarded as a failure.
				break
			}
			cb.RecordResult(chunk.Error)
			errJSON, _ := json.Marshal(ai.FriendlyProviderError(chunk.Error))
			send(fmt.Sprintf("data: {\"error\": %s}\n\n", string(errJSON)))
			return
		}
		if chunk.Content != "" {
			accumulated.WriteString(chunk.Content)
			// Only emit the non-reasoning portion to the client.
			visible := thinkFilter.Feed(chunk.Content)
			if visible != "" {
				escaped := strings.ReplaceAll(visible, "\n", "\\n")
				escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
				send(fmt.Sprintf("data: {\"content\": \"%s\"}\n\n", escaped))
			}
		}
		if chunk.Done {
			break
		}
	}

	// Post-stream: parse tool calls and sanitize
	fullResponse := accumulated.String()
	cleanText, proposedActions := ai.ParseToolCalls(fullResponse)
	sanitized := business.SanitizeResponse(cleanText)

	// Safety net: drop tool calls for conversational inputs
	if isConversational {
		proposedActions = nil
	}

	// Agent read-loop: when the model requested read-only tools, auto-execute
	// them, feed the results back, and let it propose any follow-up write
	// actions — all bounded and resiliency-gated. Write actions are never
	// auto-run; they are surfaced below for explicit confirmation. This is a
	// no-op (identical to single-shot) when no read-only tool was emitted.
	var writeActions []ai.ProposedAction
	if len(proposedActions) > 0 && !lt.Stopped() {
		reads, writes := ai.ClassifyActions(proposedActions)
		if len(reads) > 0 {
			var finalText string
			finalText, writeActions = business.RunAgentReadLoop(lt.Gen, &userInfo, messages, sanitized, proposedActions, opts, localization, llm, cb)
			sanitized = finalText
		} else {
			writeActions = writes
		}
	}

	// Tell the client when this answer was built from a shortened prompt. Emitted
	// after the text so it reads as a footnote on a finished answer rather than a
	// warning about one that is still arriving, and omitted entirely when nothing was
	// cut — a notice that appears on every answer is a notice nobody reads.
	if notice := ai.TakeContextNotice(ctx); notice.Any() {
		if noticeJSON, merr := json.Marshal(map[string]any{"notice": notice.Message(), "context_notice": notice}); merr == nil {
			send(fmt.Sprintf("data: %s\n\n", string(noticeJSON)))
		}
	}

	// Send a "replace" event so the frontend can swap in the sanitized text
	// (this removes any leaked tool_call blocks or UUIDs that streamed through)
	if sanitized != fullResponse {
		replaceEvent := map[string]string{"replace": sanitized}
		replaceJSON, marshalErr := json.Marshal(replaceEvent)
		if marshalErr == nil {
			send(fmt.Sprintf("data: %s\n\n", string(replaceJSON)))
		}
	}

	// Send proposed WRITE actions (read tools already ran in the loop above).
	// These are the only actions that require explicit user confirmation.
	if len(writeActions) > 0 {
		// Repair any *_uuid the model mis-transcribed from the grounding
		// sources (covers the no-read path, e.g. "post X in #general").
		// Conservative: only fixes malformed ids, so it never overrides a
		// value the read-loop already validated.
		if len(contextSources) > 0 {
			business.ReconcileWriteActionsWithSources(writeActions, contextSources)
		}
		actionsJSON, err := json.Marshal(writeActions)
		if err == nil {
			send(fmt.Sprintf("data: {\"actions\": %s}\n\n", string(actionsJSON)))
		}
	}

	// Send grounding sources (citations) so the UI can show what the answer
	// was based on. Only when the answer actually used workspace context and
	// wasn't a refusal/empty — gives users a trust signal + deep links.
	if len(contextSources) > 0 && strings.TrimSpace(sanitized) != "" {
		if sourcesJSON, err := json.Marshal(map[string]any{"sources": contextSources}); err == nil {
			send(fmt.Sprintf("data: %s\n\n", string(sourcesJSON)))
		}
	}

	// Send done signal
	send("data: {\"done\": true}\n\n")
	if !lt.Stopped() {
		svc.Resiliency.CB.RecordSuccess()
	}

	// Save sanitized version to session.
	//
	// Two stores, two jobs. Redis keeps the working history the next prompt is
	// built from, under a short TTL and a tight message cap, which is right for
	// token control and wrong for memory: a conversation vanished half an hour
	// after the last message and nothing indexed it, so there was no list to
	// return to and nothing to return to it with.
	//
	// SYNCHRONOUS, WHERE IT USED TO BE A GOROUTINE. The client already has its
	// done event, so nothing waits on this but the handler's return, and the
	// deferred lt.Close is what tells a returning client the answer is there:
	// it has to run after the record is written, not before. A client that
	// left mid-answer is polling for exactly this moment.
	if sessionID != "" && strings.TrimSpace(sanitized) != "" {
		bgCtx := context.Background()
		_ = ai.AppendToSession(bgCtx, sessionID, userInfo.UserDgraphInfo.Uuid, req.Question, sanitized)
		if parsed, perr := uuid.Parse(sessionID); perr == nil {
			chatSessionBusiness.Record(bgCtx, parsed, userInfo.UserPostgresInfo.Id, req.Question, sanitized)
		}
	}
}

// StopAIStream handles POST /ai/ask/stop
//
// The other half of a stream that outlives its connection: the way the person
// it belongs to says "enough". Recorded against the session and honoured only
// by a stream they own; the answer written so far is kept.
func StopAIStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.SessionID) == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "session_id is required"})
		return
	}
	if err := ai.RequestStreamStop(ctx, strings.TrimSpace(req.SessionID), userInfo.UserDgraphInfo.Uuid); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "could not record the stop"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// durableHistory reads a conversation back from Postgres in the shape the
// prompt builder wants. Ownership is enforced in the query, so a session id
// belonging to someone else returns nothing rather than their conversation.
func durableHistory(ctx context.Context, sessionID string, userID uuid.UUID) []ai.ChatMessage {
	parsed, err := uuid.Parse(sessionID)
	if err != nil {
		return nil
	}
	stored, err := chatSessionBusiness.Messages(ctx, parsed, userID)
	if err != nil || len(stored) == 0 {
		return nil
	}
	history := make([]ai.ChatMessage, 0, len(stored))
	for _, m := range stored {
		history = append(history, ai.ChatMessage{Role: m.Role, Content: m.Content})
	}
	return history
}

// ListChatSessions handles GET /ai/sessions
//
// The list that could not be built before: nothing indexed a person's
// conversations, so there was nothing to enumerate.
func ListChatSessions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Unauthorized"})
		return
	}
	sessions, err := chatSessionBusiness.List(ctx, userInfo.UserPostgresInfo.Id, 50)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to load conversations"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": sessions})
}

// GetChatSession handles GET /ai/sessions/{session_id}
//
// Ownership is enforced in the query rather than checked here, so reading
// somebody else's conversation by guessing a uuid is not possible even if this
// handler is later refactored carelessly.
func GetChatSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Unauthorized"})
		return
	}
	sessionID, perr := uuid.Parse(chi.URLParam(r, "session_id"))
	if perr != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid conversation id"})
		return
	}
	messages, err := chatSessionBusiness.Messages(ctx, sessionID, userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to load the conversation"})
		return
	}
	// live tells a client that came back mid-answer that there is something to
	// wait for: the exchange is not in messages yet because it is recorded when
	// the answer finishes, and the client would otherwise show the conversation
	// as if the question had never been asked.
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"data": messages,
		"live": ai.StreamLive(ctx, sessionID.String()),
	})
}

// DeleteChatSession handles DELETE /ai/sessions/{session_id}
func DeleteChatSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Unauthorized"})
		return
	}
	sessionID, perr := uuid.Parse(chi.URLParam(r, "session_id"))
	if perr != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid conversation id"})
		return
	}
	if err := chatSessionBusiness.Delete(ctx, sessionID, userInfo.UserPostgresInfo.Id); err != nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "Conversation not found"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Conversation deleted"})
}

// GetAIStatus handles GET /ai/status
// Returns the health and configuration of the AI service.
func GetAIStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	svc := ai.GetService()
	cfg := ai.GetConfig()
	if svc == nil || cfg == nil {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
			"msg": "AI service status",
			"data": adapter.AIStatusResponse{
				Enabled: false,
			},
		})
		return
	}

	status := adapter.AIStatusResponse{
		Enabled:            cfg.Enabled,
		Provider:           string(cfg.Provider()),
		Model:              cfg.ActiveModel(),
		EmbeddingModel:     cfg.ActiveEmbeddingModel(),
		CircuitState:       svc.Resiliency.CB.State(),
		RateLimitRemaining: svc.Resiliency.GetRateLimitRemaining(ctx, userInfo.UserDgraphInfo.Uuid),
		WebSearchEnabled:   ai.WebSearchEnabled(),
		SandboxEnabled:     ai.SandboxEnabled(),
		CodePREnabled:      ai.CodePREnabled(),
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "AI service status",
		"data": status,
	})
}

// GetAIUsage handles GET /ai/usage. Returns today's AI token spend for the
// workspace and the calling user, with their configured daily caps (0 =
// unlimited). Read-only; lets the admin panel show consumption and a user see
// how close they are to their own limit.
func GetAIUsage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "AI usage",
		"data": ai.CurrentUsage(ctx),
	})
}

// userUsageRow is one entry in the admin "top consumers today" breakdown:
// today's token spend for a user with their resolved display name.
type userUsageRow struct {
	UserID   string `json:"user_id"`
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Used     int64  `json:"used"`
}

// GetAIUserUsage handles GET /ai/usage/users (admin-only, via the admin AI
// config route group). Returns today's highest AI-token consumers so an admin
// can see who is spending the workspace budget and set sensible per-user caps.
// Read-only and best-effort: an empty list when Redis is unavailable.
func GetAIUserUsage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	limit := 25
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	top := ai.TopUserUsage(ctx, limit)
	ids := make([]string, 0, len(top))
	for _, t := range top {
		ids = append(ids, t.UserID)
	}
	displays, _ := userDomain.ResolveUserDisplays(ctx, ids)

	rows := make([]userUsageRow, 0, len(top))
	for _, t := range top {
		row := userUsageRow{UserID: t.UserID, Used: t.Used}
		if d, ok := displays[t.UserID]; ok {
			row.FullName = d.FullName
			row.Name = d.Name
		}
		rows = append(rows, row)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "AI usage by user",
		"data": helpers.Envolope{"day": ai.CurrentUsage(ctx).Day, "users": rows},
	})
}

// channelUsageRow is one entry in the admin "top AI-spending channels today"
// breakdown: a channel's token spend with its resolved name.
type channelUsageRow struct {
	ChannelID string `json:"channel_id"`
	Name      string `json:"name"`
	Used      int64  `json:"used"`
}

// GetAIChannelUsage handles GET /ai/usage/channels (admin-only). Returns
// today's highest AI-spending channels so an admin can see where the AI cost is
// going and set per-channel caps (Claude-Tag's per-channel usage breakdown).
// Read-only and best-effort: an empty list when Redis is unavailable.
func GetAIChannelUsage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	limit := 25
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	top := ai.TopChannelUsage(ctx, limit)
	ids := make([]string, 0, len(top))
	for _, t := range top {
		ids = append(ids, t.ChannelID)
	}
	names, _ := channelDomain.GetChannelNamesByUUIDs(ctx, ids)

	rows := make([]channelUsageRow, 0, len(top))
	for _, t := range top {
		row := channelUsageRow{ChannelID: t.ChannelID, Used: t.Used}
		if n, ok := names[t.ChannelID]; ok {
			row.Name = n
		}
		rows = append(rows, row)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":  "AI usage by channel",
		"data": helpers.Envolope{"day": ai.CurrentUsage(ctx).Day, "channels": rows},
	})
}

// ListMyModels handles GET /ai/models
// Returns the models a member may choose from (admin-authorized + provider
// enabled) plus their current selection. Empty selection = workspace default.
func ListMyModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	resp, err := business.ListUsableModelsForUser(ctx, userInfo.UserDgraphInfo.Uuid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": resp})
}

// SetMyModel handles POST /ai/model-preference
// Sets (or clears, when model_id is empty) the member's chosen model.
func SetMyModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.SetUserModelPreferenceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}

	var modelID *uuid.UUID
	if trimmed := strings.TrimSpace(req.ModelID); trimmed != "" {
		id, err := uuid.Parse(trimmed)
		if err != nil {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid model id"})
			return
		}
		modelID = &id
	}

	if err := business.SetUserModelPreference(ctx, userInfo.UserDgraphInfo.Uuid, modelID); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "model preference updated"})
}

// GetMyAIInstructions handles GET /ai/instructions — the member's personal AI
// custom instructions ("" when unset). Member-accessible, self-scoped.
func GetMyAIInstructions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	instructions, err := business.GetUserCustomInstructions(ctx, userInfo.UserDgraphInfo.Uuid)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": helpers.Envolope{"instructions": instructions}})
}

// SetMyAIInstructions handles POST /ai/instructions — set/clear the member's
// personal AI custom instructions. Self-scoped; the business layer trims and
// length-caps.
func SetMyAIInstructions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req struct {
		Instructions string `json:"instructions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if err := business.SetUserCustomInstructions(ctx, userInfo.UserDgraphInfo.Uuid, req.Instructions); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "msg": "AI instructions updated"})
}

// AnalyzeCodeIssue handles POST /ai/code/analyze
// Runs the code-aware agent: retrieves the relevant repo files via the GitHub
// API and returns a root-cause + proposed patch for the given issue/error. The
// result is returned to the workspace for review; nothing is written to GitHub.
func AnalyzeCodeIssue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.AnalyzeCodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	req.Owner = strings.TrimSpace(req.Owner)
	req.Repo = strings.TrimSpace(req.Repo)
	if req.Owner == "" || req.Repo == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "owner and repo are required"})
		return
	}
	if strings.TrimSpace(req.Title) == "" && strings.TrimSpace(req.Body) == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "issue title or body is required"})
		return
	}

	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "AI service is not enabled"})
		return
	}
	// Rate-limit per user (the analysis itself gates on the provider breaker).
	if err := svc.Resiliency.CheckRateLimit(ctx, userInfo.UserDgraphInfo.Uuid); err != nil {
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": err.Error()})
		return
	}

	analysis, err := codeagent.AnalyzeIssue(ctx, req.Owner, req.Repo, req.Title, req.Body, req.Ref, req.Deep)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/AnalyzeCodeIssue failed: %v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"data": adapter.AnalyzeCodeResponse{
			Answer:          analysis.Answer,
			FilesConsidered: analysis.FilesConsidered,
			Partial:         analysis.Partial,
		},
	})
}

// DraftReleaseNotes handles POST /ai/release-notes
// Drafts user-facing release notes from PRs merged on a repo in the last N
// days. Read-only against GitHub; returns a draft for the user to edit/publish.
func DraftReleaseNotes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.ReleaseNotesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	req.Owner = strings.TrimSpace(req.Owner)
	req.Repo = strings.TrimSpace(req.Repo)
	if req.Owner == "" || req.Repo == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "repo owner and name are required"})
		return
	}

	res, err := business.DraftReleaseNotes(ctx, userInfo.UserDgraphInfo.Uuid, req.Owner, req.Repo, req.Days)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/DraftReleaseNotes failed: %v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"data": adapter.ReleaseNotesResponse{
			Notes:   res.Notes,
			PRCount: res.PRCount,
			Days:    res.Days,
		},
	})
}

// DraftSocialPosts handles POST /ai/social-posts
// Drafts platform-tailored social copy (X tweet/thread, Reddit, ...) for a
// topic. Returns drafts to review and post; nothing is published.
func DraftSocialPosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	var req adapter.SocialPostsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "invalid request body"})
		return
	}
	if strings.TrimSpace(req.Topic) == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "describe what you want to post about"})
		return
	}

	posts, err := business.DraftSocialPosts(ctx, userInfo.UserDgraphInfo.Uuid, req.Topic, req.Platforms)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/DraftSocialPosts failed: %v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": err.Error()})
		return
	}

	out := make([]adapter.SocialPostView, 0, len(posts))
	for _, p := range posts {
		out = append(out, adapter.SocialPostView{Platform: p.Platform, Label: p.Label, Content: p.Content})
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": out})
}
