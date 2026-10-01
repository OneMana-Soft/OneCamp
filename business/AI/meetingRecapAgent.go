package business

// Meeting Recap — OneCamp's first ambient AI agent.
//
// When a call ends (LiveKit `room_finished` webhook), this agent turns
// the call's transcript into a concise recap (summary + decisions +
// action items) and posts it back to the exact channel / DM / group chat
// where the call happened. This is the kind of workspace-aware automation
// a chat-only tool structurally cannot do: it requires owning the call,
// the transcript, AND the destination surface in one system.
//
// Design / safety:
//   - Opt-in: only runs when ai_settings.meeting_recap_enabled is true AND
//     AI is enabled.
//   - Idempotent: a short-lived Redis lock per (room, egress) prevents a
//     duplicate webhook (room_finished + participant_left backstop) from
//     posting two recaps.
//   - Bounded: transcript line count and prompt size are capped; the LLM
//     call goes through the same circuit breaker as the rest of AI.
//   - Permission-correct: the recap is authored by a real call
//     participant, so it lands with that user's existing access to the
//     channel/group — no synthetic bot identity that could leak content
//     to someone without access.
//   - Best-effort: every failure path logs and returns; a recap never
//     blocks or breaks call teardown.

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	botpost "github.com/akashc777/OneCamp/business/BotPost"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	transcriptDomain "github.com/akashc777/OneCamp/domain/LiveKit"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	// Minimum transcript utterances to bother summarizing — a 2-line call
	// isn't worth a recap and produces noise.
	minRecapLines = 6
	// Cap lines fed to the model to bound prompt size / cost.
	maxRecapLines = 4000
	// Cap characters in the assembled transcript prompt.
	maxRecapPromptChars = 24000
)

// recapSystemPrompt is tuned for meeting recaps: factual, structured,
// never inventing content. Kept strict for small local models.
const recapSystemPrompt = `You are OneCamp's meeting assistant. You are given the transcript of a voice/video call.
Produce a concise recap with EXACTLY these sections, in this order, using markdown:

**📝 Summary**
- 2-4 bullet points capturing what the call was about and key outcomes.

**✅ Decisions**
- Each concrete decision made. If none, write "- None".

**📋 Action Items**
- Each task someone committed to, prefixed with the owner when stated (e.g. "@name: do X by Friday"). If none, write "- None".

Rules:
- The transcript may be in ANY language, or mix several. Understand it regardless of language.
- ALWAYS write the recap in English. If the transcript is in another language, translate the summary, decisions, and action items faithfully into English. Keep people's names and proper nouns (products, companies) as-is; do not translate or transliterate them.
- Use ONLY information present in the transcript. Never invent names, dates, or commitments.
- Be concise. No preamble, no closing remarks.
- If the transcript is too short or unintelligible to summarize, reply with exactly: SKIP_RECAP`

// composeRecapPrompt layers optional admin custom instructions onto the base
// recap system prompt. When instructions are blank the base prompt is returned
// verbatim (default behavior). The custom block is clearly delimited and placed
// AFTER the base rules, so a workspace can add emphasis (a "Risks" section, an
// output language, a compliance flag) without being able to override the
// grounding rules (use only the transcript, never invent) or the SKIP_RECAP
// contract that the base prompt establishes. Pure + unit-tested.
func composeRecapPrompt(customInstructions string) string {
	custom := strings.TrimSpace(customInstructions)
	if custom == "" {
		return recapSystemPrompt
	}
	if len(custom) > maxRecapInstructionsLen {
		custom = custom[:maxRecapInstructionsLen]
	}
	return recapSystemPrompt + "\n\nAdditional workspace instructions (apply where they do not conflict with the rules above):\n" + custom
}

// MaybeRunMeetingRecap is the entry point called from the LiveKit
// room_finished webhook. It is safe to call unconditionally and on every
// finished room; it self-gates on settings, idempotency, and content.
// Fire-and-forget: callers should invoke it in a goroutine.
func MaybeRunMeetingRecap(roomName string) {
	go func() {
		// Generous standalone context — call teardown must not be coupled
		// to recap latency, and local LLMs can be slow.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		if err := runMeetingRecap(ctx, roomName); err != nil {
			helpers.LogErrorWithContext(ctx, "AI meeting recap for room %q: %v", roomName, err)
		}
	}()
}

func runMeetingRecap(ctx context.Context, roomName string) error {
	// 1. Gate on settings.
	settings, err := aiModels.GetSettings(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	if !settings.Enabled || !settings.MeetingRecapEnabled {
		return nil // feature off — silent no-op
	}

	svc := ai.GetService()
	if !svc.IsEnabled() {
		return nil
	}

	// 2. Idempotency lock: first finisher wins for this room. The webhook
	//    can fire from both room_finished and participant_left paths.
	if !acquireRecapLock(ctx, roomName) {
		helpers.LogInfoWithContext(ctx, "AI meeting recap already handled for room %q; skipping", roomName)
		return nil
	}

	// 3. Gather transcript (and the recording's egress id, so the recap can
	//    link to the playable recording).
	lines, egressID, err := transcriptDomain.GetTranscriptLinesByRoom(ctx, roomName, maxRecapLines)
	if err != nil {
		return fmt.Errorf("fetch transcript: %w", err)
	}
	// No transcript at all is a different event from a short one, and worth
	// saying so separately.
	//
	// This used to say "the call was not recorded", which was the right answer
	// when lines were only written while a recording ran. They are not any more:
	// an unrecorded call files its transcript under a call session key. So an
	// empty transcript now means nobody spoke, or transcription is off, or the
	// agent never joined, and claiming to know which would be guessing.
	if len(lines) == 0 {
		helpers.LogInfoWithContext(ctx, "AI meeting recap: room %q produced no transcript (nobody spoke, or transcription is off); skipping", roomName)
		return nil
	}
	if len(lines) < minRecapLines {
		helpers.LogInfoWithContext(ctx, "AI meeting recap: room %q has %d lines (<%d); skipping", roomName, len(lines), minRecapLines)
		return nil
	}

	// 4. Resolve participant identities → display names (cached per call).
	nameByUID := resolveParticipantNames(ctx, lines)

	// 5. Build the transcript prompt, bounded.
	transcript, speakerUIDs := formatTranscriptForRecap(lines, nameByUID)
	if strings.TrimSpace(transcript) == "" {
		return nil
	}

	// 6. Summarize via the active LLM, through the circuit breaker. The base
	//    recap prompt is layered with any admin custom instructions.
	if err := svc.Resiliency.CB.Allow(); err != nil {
		return fmt.Errorf("circuit open, skipping recap: %w", err)
	}
	recap, err := svc.SummarizeFor(ctx, ai.PurposeMeetings, transcript, composeRecapPrompt(settings.MeetingRecapInstructions))
	if err != nil {
		svc.Resiliency.CB.RecordResult(err)
		return fmt.Errorf("summarize: %w", err)
	}
	svc.Resiliency.CB.RecordSuccess()

	recap = strings.TrimSpace(recap)
	if recap == "" || strings.Contains(recap, "SKIP_RECAP") {
		helpers.LogInfoWithContext(ctx, "AI meeting recap: model returned SKIP for room %q", roomName)
		return nil
	}
	// Defense-in-depth: strip any tool/UUID leakage the same way Q&A does.
	recap = SanitizeResponse(recap)
	if recap == "" {
		return nil
	}

	// 7-8. Deliver the recap, authored by the first participant who
	// actually has access to the surface. Trying candidates in order makes
	// delivery resilient to a lead speaker who left the workspace or was
	// removed from the channel after the call.
	if len(speakerUIDs) == 0 {
		return fmt.Errorf("no resolvable participant to author recap")
	}
	// 8. The notes document (opt-in), BEFORE delivery, so the message can link
	//    to it. A document nobody can reach from the call it describes is a
	//    document nobody finds.
	//
	//    Ordering it first costs nothing it used to protect: every failure here
	//    is swallowed into an empty id, so the recap is delivered either way and
	//    the only difference is whether it carries a link. What must never
	//    happen is the document becoming a reason the message does not arrive.
	// What the transcript could not hear, said once, in both places it is
	// written down.
	gapNote := transcriptGapNote(ctx, roomName)

	notesDocUUID := ""
	if settings.MeetingNotesDocEnabled {
		docUUID, derr := createMeetingNotesDoc(ctx, meetingSurfaceName(ctx, roomName, helpers.FirstNonBlank(speakerUIDs...)), roomName, speakerUIDs, recap, gapNote, lines, nameByUID)
		if derr != nil {
			helpers.LogErrorWithContext(ctx, "AI meeting recap: notes document for room %q failed: %+v", roomName, derr)
		} else if docUUID != "" {
			notesDocUUID = docUUID
			helpers.LogInfoWithContext(ctx, "AI meeting recap: notes document %s written for room %q", docUUID, roomName)
		}
	}

	authorUUID, err := deliverRecapWithFallback(ctx, roomName, speakerUIDs, recap, egressID, notesDocUUID, gapNote)
	if err != nil {
		return err
	}

	// 10. Workspace Memory (opt-in): extract durable decisions / commitments
	//    / open questions from the transcript into the structured memory
	//    layer. Best-effort — a recap is still valuable even if extraction
	//    is off or fails. Uses the recap text (already concise + factual)
	//    rather than the raw transcript to keep the extraction prompt small
	//    and high-signal.
	extractMemoryFromRecap(ctx, roomName, authorUUID, recap)
	return nil
}

// firstNonEmpty returns the first non-blank string, or "".
//
// The channel lookup for the document title needs SOME viewer to read as, and
// any speaker will do: the title falls back cleanly when the lookup fails, so
// this never needs to be the "right" one.

// extractMemoryFromRecap feeds the recap into the memory extractor with
// scope derived from the room name. Best-effort; logs and returns.
func extractMemoryFromRecap(ctx context.Context, roomName, authorDgraphUID, recap string) {
	scope := MemoryScope{
		SourceType: "recap",
		SourceUUID: memoryRoomSourceID(roomName),
	}
	// Resolve the author's app UUID for created_by / owner-visibility.
	if u, err := getUserInfoByDgraphUID(ctx, authorDgraphUID); err == nil && u != nil {
		scope.CreatedByUUID = u.UserDgraphInfo.Uuid
	}

	switch {
	case strings.Contains(roomName, " "):
		// DM room name IS the canonical grouping id (space-joined sorted
		// user UUIDs — see helpers.GetGroupingId). Use it verbatim so the
		// scope key matches how DMs are scoped everywhere else.
		scope.ChatGrpID = strings.TrimSpace(roomName)
	case !strings.Contains(roomName, "-") && len(roomName) == 32:
		scope.ChatGrpID = roomName
	default:
		scope.ChannelUUID = roomName
	}

	if _, err := MaybeExtractMemory(ctx, recap, scope); err != nil {
		helpers.LogErrorWithContext(ctx, "AI memory extraction for room %q: %v", roomName, err)
	}
}

// memoryRoomSourceID returns a stable provenance id for a room's recap.
func memoryRoomSourceID(roomName string) string {
	return "recap:" + roomName
}

// acquireRecapLock sets a one-shot marker; returns true only for the first
// caller. TTL bounds the lock so a crash mid-recap doesn't permanently
// suppress a retry on a later (manual) trigger.
func acquireRecapLock(ctx context.Context, roomName string) bool {
	// Reuse the AIAdminRate fixed-window primitive as a 1-permit lock:
	// allow exactly 1 in the window. A dedicated SETNX would be cleaner;
	// this stays within the existing store helpers.
	res := redisStore.AllowFixedWindow(ctx, registry.AIRecapLock, []string{roomName}, 1)
	return res.Allowed
}

// resolveParticipantNames maps distinct participant Dgraph UIDs to display
// names. The transcription agent's own identity (non-human) is skipped.
func resolveParticipantNames(ctx context.Context, lines []transcriptDomain.TranscriptLine) map[string]string {
	out := make(map[string]string)
	for _, l := range lines {
		uid := l.ParticipantIdentity
		if uid == "" || out[uid] != "" {
			continue
		}
		// Resolve by Dgraph node uid (the identity LiveKit tokens carry).
		if u, err := userDomain.GetDgraphUserInfoByDgraphUID(ctx, uid); err == nil && u != nil && u.UserName != "" {
			out[uid] = u.UserName
		} else {
			out[uid] = "participant"
		}
	}
	return out
}

// formatTranscriptForRecap renders the transcript as "Name: text" lines,
// bounded by maxRecapPromptChars, and returns the distinct speaker UIDs
// in first-seen order.
func formatTranscriptForRecap(lines []transcriptDomain.TranscriptLine, nameByUID map[string]string) (string, []string) {
	var sb strings.Builder
	seen := map[string]bool{}
	var speakerUIDs []string

	for _, l := range lines {
		text := strings.TrimSpace(l.Text)
		if text == "" {
			continue
		}
		name := nameByUID[l.ParticipantIdentity]
		if name == "" {
			name = "participant"
		}
		if !seen[l.ParticipantIdentity] && isLikelyUserUID(l.ParticipantIdentity) {
			seen[l.ParticipantIdentity] = true
			speakerUIDs = append(speakerUIDs, l.ParticipantIdentity)
		}
		line := fmt.Sprintf("%s: %s\n", name, text)
		if sb.Len()+len(line) > maxRecapPromptChars {
			break
		}
		sb.WriteString(line)
	}
	return sb.String(), speakerUIDs
}

// isLikelyUserUID filters out the transcription agent's synthetic
// identity so it's never chosen as the recap author.
func isLikelyUserUID(identity string) bool {
	if identity == "" {
		return false
	}
	// The agent identity is recognized by the LiveKit business helper.
	return !liveKitBusiness.IsAgentIdentity(identity)
}

// deliverRecapWithFallback attempts delivery authored by each candidate
// participant (Dgraph UIDs, first-seen order) until one succeeds — i.e.
// one who still has access to the surface. Returns the successful author's
// Dgraph UID, or an error if none could deliver. This makes the recap
// resilient to a lead speaker who left the workspace / lost channel access
// after the call, without ever posting on behalf of someone without access
// (each delivery path re-checks the author's access).
func deliverRecapWithFallback(ctx context.Context, roomName string, speakerUIDs []string, recap, egressID, notesDocUUID, gapNote string) (string, error) {
	var lastErr error
	tried := 0
	for _, uid := range speakerUIDs {
		// Cap attempts so a pathological speaker list can't fan out into
		// many DB round-trips.
		if tried >= 5 {
			break
		}
		tried++
		if err := deliverRecap(ctx, roomName, uid, recap, egressID, notesDocUUID, gapNote); err != nil {
			lastErr = err
			helpers.LogInfoWithContext(ctx, "AI recap: author %s could not deliver to %q (%v); trying next", uid, roomName, err)
			continue
		}
		return uid, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no candidate could author the recap")
	}
	return "", lastErr
}

// deliverRecap posts the recap to the correct surface. Room-name shape
// (mirrors controllers/LiveKit.broadcastCallStop):
//   - contains a space   → DM (space-separated sorted user UUIDs)
//   - 32-char hex hash    → group chat (grouping id)
//   - UUID (hyphens)      → channel
func deliverRecap(ctx context.Context, roomName, authorUID, recap, egressID, notesDocUUID, gapNote string) error {
	authorInfo, err := getUserInfoByDgraphUID(ctx, authorUID)
	if err != nil {
		return fmt.Errorf("resolve author: %w", err)
	}

	htmlText := recapToHTML(recap, recordingPlayAttrs(roomName, egressID), notesDocUUID, gapNote)

	switch {
	case strings.Contains(roomName, " "):
		return deliverRecapToDM(ctx, roomName, authorInfo, htmlText)
	case !strings.Contains(roomName, "-") && len(roomName) == 32:
		return deliverRecapToGroup(ctx, roomName, authorInfo, htmlText)
	default:
		return deliverRecapToChannel(ctx, roomName, authorInfo, htmlText)
	}
}

// getUserInfoByDgraphUID builds a full UserInfo (postgres + dgraph) from a
// Dgraph node uid. The recap agent authors posts as a real call
// participant, so this must yield a user with genuine workspace access.
func getUserInfoByDgraphUID(ctx context.Context, dgraphUID string) (*userModels.UserInfo, error) {
	dgraphUser, err := userDomain.GetDgraphUserInfoByDgraphUID(ctx, dgraphUID)
	if err != nil {
		return nil, fmt.Errorf("dgraph lookup: %w", err)
	}
	if dgraphUser == nil || dgraphUser.Uuid == "" {
		return nil, fmt.Errorf("no user for dgraph uid %q", dgraphUID)
	}
	// Reuse the executor's UUID-based full-info builder.
	return getUserInfoForExecutor(ctx, dgraphUser.Uuid)
}

// deliverRecapToChannel posts the recap as a channel post. It prefers authoring
// as the shared automation bot ("OneCamp AI") — the consistent identity every
// other automated message uses — resolving the channel via the membership-free
// basic resolver (the bot is a synthetic non-member, like webhooks/workflows
// post as it). If the bot is unavailable, it falls back to authoring as the
// call participant (whose access was already re-checked by the caller).
// CreatePost stores the HTML verbatim, so the recording-embed "Play recording"
// node is preserved (unlike botpost, which would sanitize it away).
func deliverRecapToChannel(ctx context.Context, channelUUID string, authorInfo *userModels.UserInfo, htmlText string) error {
	parsed, err := uuid.Parse(channelUUID)
	if err != nil {
		return fmt.Errorf("invalid channel room name %q: %w", channelUUID, err)
	}

	// Preferred: author as the automation bot.
	if bot := userBusiness.GetAutomationBot(ctx); bot != nil && bot.DgraphUID != "" {
		if botInfo, berr := getUserInfoForExecutor(ctx, bot.UUID); berr == nil && botInfo != nil {
			if dgraphChannel, cerr := channelDomain.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, bot.DgraphUID); cerr == nil && dgraphChannel != nil {
				postInfo := &postAdapter.InputCreateOrUpdatePostInfo{
					HTMLText:    htmlText,
					ChannelUuid: channelUUID,
					ChannelUUID: parsed,
				}
				if _, perr := postBusiness.CreatePost(ctx, postInfo, botInfo, nil, dgraphChannel); perr == nil {
					helpers.LogInfoWithContext(ctx, "AI meeting recap posted to channel %q as the automation bot", channelUUID)
					return nil
				} else {
					helpers.LogErrorWithContext(ctx, "AI meeting recap: bot post to channel %q failed, falling back to participant: %v", channelUUID, perr)
				}
			}
		}
	}

	// Fallback: author as the call participant (access re-checked here).
	dgraphChannel, err := channelDomain.GetDgraphChannelInfoByUUID(ctx, channelUUID, authorInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphChannel == nil {
		return fmt.Errorf("recap author lacks access to channel %q", channelUUID)
	}
	postInfo := &postAdapter.InputCreateOrUpdatePostInfo{
		HTMLText:    htmlText,
		ChannelUuid: channelUUID,
		ChannelUUID: parsed,
	}
	if _, err := postBusiness.CreatePost(ctx, postInfo, authorInfo, nil, dgraphChannel); err != nil {
		return fmt.Errorf("post recap to channel: %w", err)
	}
	helpers.LogInfoWithContext(ctx, "AI meeting recap posted to channel %q", channelUUID)
	return nil
}

// deliverRecapToGroup posts the recap into a group chat. It resolves the group
// (and its participant list) via the call participant — confirming the group is
// real and the participant is a member — but authors the message as the shared
// automation bot ("OneCamp AI") when available, so the recap carries the same
// consistent AI identity as channel recaps. CreateChatForGroup records the
// author into the existing grouping and notifies the resolved participants, so
// the bot need not be a group member. Falls back to the participant as author
// if the bot is unavailable.
func deliverRecapToGroup(ctx context.Context, grpID string, authorInfo *userModels.UserInfo, htmlText string) error {
	dgraphDm, err := chatBusiness.GetDgraphDmBasicInfoFromDgraph(ctx, authorInfo.UserDgraphInfo.Uid, grpID)
	if err != nil || dgraphDm == nil {
		return fmt.Errorf("recap author lacks access to group %q", grpID)
	}
	if dgraphDm.ParticipantIsMember == 0 {
		return fmt.Errorf("recap author not a member of group %q", grpID)
	}

	author := authorInfo
	if bot := userBusiness.GetAutomationBot(ctx); bot != nil && bot.UUID != "" {
		if botInfo, berr := getUserInfoForExecutor(ctx, bot.UUID); berr == nil && botInfo != nil {
			author = botInfo
		}
	}

	chatInput := &chatAdapter.ChatInfo{TextHtml: htmlText, GrpUuid: grpID}
	if _, err := chatBusiness.CreateChatForGroup(ctx, chatInput, author, nil, dgraphDm.Participants); err != nil {
		return fmt.Errorf("post recap to group: %w", err)
	}
	helpers.LogInfoWithContext(ctx, "AI meeting recap posted to group %q", grpID)
	return nil
}

// deliverRecapToDM posts the recap into a 1:1 DM. The room name is the
// space-joined sorted pair of user UUIDs; the recipient is the OTHER
// participant relative to the author.
func deliverRecapToDM(ctx context.Context, roomName string, authorInfo *userModels.UserInfo, htmlText string) error {
	parts := strings.Fields(roomName)
	if len(parts) != 2 {
		return fmt.Errorf("unexpected DM room name %q", roomName)
	}
	authorPgID := authorInfo.UserPostgresInfo.Id.String()
	toUUID := ""
	for _, p := range parts {
		if p != authorInfo.UserDgraphInfo.Uuid && p != authorPgID {
			toUUID = p
		}
	}
	if toUUID == "" {
		return fmt.Errorf("could not determine DM recipient from room %q", roomName)
	}
	parsedTo, err := uuid.Parse(toUUID)
	if err != nil {
		return fmt.Errorf("invalid DM recipient %q: %w", toUUID, err)
	}
	sendTo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, toUUID)
	if err != nil || sendTo == nil {
		return fmt.Errorf("DM recipient not found: %w", err)
	}
	chatInput := &chatAdapter.ChatInfo{TextHtml: htmlText, ToUuid: toUUID}
	if _, err := chatBusiness.CreateChat(ctx, chatInput, authorInfo, sendTo, parsedTo, nil); err != nil {
		return fmt.Errorf("post recap to DM: %w", err)
	}
	helpers.LogInfoWithContext(ctx, "AI meeting recap posted to DM %q", roomName)
	return nil
}

// recordingPlayLink builds the in-app data attributes for the recap's "Play
// recording" block (a recordingEmbed Tiptap node). The FE renders it as a
// button that opens the recording player in place (no navigation). Returns ""
// when there's no egress id.
//
// Surface attrs mirror the room-name shape:
//   - channel → kind=channel & sid=<channelUUID>
//   - group   → kind=group   & sid=<groupId>
//   - DM      → kind=dm & u1=<uuidA> & u2=<uuidB> (the FE picks the peer ≠ self,
//     since a DM recording media URL is addressed by the OTHER participant)
func recordingPlayAttrs(roomName, egressID string) string {
	id := strings.TrimSpace(egressID)
	if id == "" {
		return ""
	}
	attrs := fmt.Sprintf(` data-egress="%s"`, htmlAttrEscape(id))
	switch {
	case strings.Contains(roomName, " "):
		parts := strings.Fields(roomName)
		if len(parts) == 2 {
			attrs += fmt.Sprintf(` data-kind="dm" data-u1="%s" data-u2="%s"`,
				htmlAttrEscape(parts[0]), htmlAttrEscape(parts[1]))
		}
	case !strings.Contains(roomName, "-") && len(roomName) == 32:
		attrs += fmt.Sprintf(` data-kind="group" data-sid="%s"`, htmlAttrEscape(roomName))
	default:
		attrs += fmt.Sprintf(` data-kind="channel" data-sid="%s"`, htmlAttrEscape(roomName))
	}
	return attrs
}

// transcriptGapNote describes, in one sentence, the part of the conversation
// this transcript could not hear.
//
// Returns "" when nothing is missing, which is the normal case and must stay
// silent: a caveat on every recap teaches people to skip the caveat.
//
// Only reports what is KNOWN missing, from participants whose browser told the
// server it has no speech recognizer. Someone who simply said nothing is not a
// gap, and treating them as one would put this note on most meetings.
func transcriptGapNote(ctx context.Context, roomName string) string {
	uids, err := redisStore.GetSetMembers(ctx, registry.CallUntranscribed, []string{roomName})
	if err != nil || len(uids) == 0 {
		return ""
	}
	people := "person was"
	if len(uids) > 1 {
		people = "people were"
	}
	return fmt.Sprintf(
		"Note: %d %s on a browser that cannot transcribe speech, so anything they said is missing from this transcript.",
		len(uids), people)
}

// MarkAIGenerated wraps a body so the content itself says it was generated.
//
// WHY. The AI Act's transparency obligations became applicable on 2 August 2026
// and require AI-generated content to carry a marking, machine-readable, that
// enables detection of artificial generation. OneCamp already discloses AI
// authorship through the AUTHOR: the message comes from a bot principal with a
// badge and a profile saying which kind of bot it is. That covers "tell people
// they are talking to AI" and covers nothing about the content, so a recap
// copied out of a channel carried no signal at all.
//
// A wrapper rather than a field on the record, because a marker that travels
// with the text is the one that survives being quoted, forwarded, or pasted
// into a document. The recap already emits a data-attributed div for its
// recording embed, so this shape is known to pass the message pipeline.
//
// Applied at the OUTERMOST producer of each surface rather than inside
// modelTextToHTML, so the marking covers the whole message including the parts
// this code adds around the model's words, which are equally not written by a
// person.
func MarkAIGenerated(html string) string {
	if strings.TrimSpace(html) == "" {
		return html
	}
	return `<div data-ai-generated="true">` + html + `</div>`
}

// modelTextToHTML turns model-written text into the HTML the message and
// document pipelines expect.
//
// TWO BUGS, ONE CAUSE. Model output was reaching HTML surfaces with no
// conversion at all. The recap prompt asks for markdown, so every recap ever
// posted showed its section headings as literal asterisks, "**📝 Summary**"
// rather than a bold heading. And the assistant's send-message tools wrapped
// whatever the model wrote in a single <p>, where newlines collapse to spaces,
// so a message composed as several lines or a list arrived as one run-on
// sentence. Both read like bugs in the product because they are.
//
// One function for every surface that renders model text, so they cannot format
// the same string differently again.
//
// Deliberately a handful of rules rather than a markdown library. The input is
// not arbitrary markdown, it is output from a prompt that asks for exactly two
// shapes, bold section headings and "- " bullets, and a full parser would bring
// link and image syntax into a string built from meeting audio. The unsupported
// rest degrades to a paragraph, which is what it does today.
//
// ESCAPE FIRST, THEN MARKUP. Tags are stripped and the text escaped before any
// element is added, so every tag in the result is one this function wrote.
//
// CHARTS FIRST. A closed ```chart block becomes a chart embed node and the
// runs of prose around it go through the rules below, so an agent that posts
// a weekly report into a channel draws the same chart it would draw in the
// assistant panel. The split runs on the raw text because the spec is JSON
// and escaping it first would hide it.
func modelTextToHTML(text string) string {
	return botpost.RenderWithCharts(text, func(run string, _ bool) string {
		return modelProseToHTML(run)
	})
}

// modelProseToHTML renders one run of model text with no chart in it.
func modelProseToHTML(text string) string {
	safe := html.EscapeString(helpers.RemoveHTMLTags(text))

	var sb strings.Builder
	// Blocks are separated by a blank line and soft newlines become <br/>,
	// matching renderParagraphs in business/BotPost, which is how streamed
	// assistant replies have always rendered. Agreeing with it matters more
	// than any improvement on it: the same sentence should not look different
	// depending on which code path produced the message.
	for _, block := range strings.Split(safe, "\n\n") {
		lines := make([]string, 0, 8)
		for _, raw := range strings.Split(block, "\n") {
			if l := strings.TrimSpace(raw); l != "" {
				lines = append(lines, l)
			}
		}
		if len(lines) == 0 {
			continue
		}
		sb.WriteString(renderModelBlock(lines))
	}
	return sb.String()
}

// renderModelBlock renders one blank-line-separated block.
//
// A block whose lines are all "- " bullets becomes a list; a single line
// wrapped entirely in ** ** becomes a heading; anything else is a paragraph
// with soft newlines preserved.
func renderModelBlock(lines []string) string {
	allBullets := true
	for _, l := range lines {
		if !strings.HasPrefix(l, "- ") {
			allBullets = false
			break
		}
	}
	if allBullets {
		var sb strings.Builder
		sb.WriteString("<ul>")
		for _, l := range lines {
			sb.WriteString("<li>" + inlineBold(strings.TrimPrefix(l, "- ")) + "</li>")
		}
		sb.WriteString("</ul>")
		return sb.String()
	}

	// A mixed block: a heading line followed by its bullets is the shape the
	// recap prompt produces, so each line is rendered on its own terms rather
	// than the block being forced into one form.
	var sb strings.Builder
	var para []string
	flush := func() {
		if len(para) == 0 {
			return
		}
		sb.WriteString("<p>" + strings.Join(para, "<br/>") + "</p>")
		para = para[:0]
	}
	var bullets []string
	flushBullets := func() {
		if len(bullets) == 0 {
			return
		}
		sb.WriteString("<ul>")
		for _, b := range bullets {
			sb.WriteString("<li>" + b + "</li>")
		}
		sb.WriteString("</ul>")
		bullets = bullets[:0]
	}
	for _, l := range lines {
		if item, ok := strings.CutPrefix(l, "- "); ok {
			flush()
			bullets = append(bullets, inlineBold(item))
			continue
		}
		flushBullets()
		if inner, ok := wholeLineBold(l); ok {
			flush()
			sb.WriteString("<p><strong>" + inner + "</strong></p>")
			continue
		}
		para = append(para, inlineBold(l))
	}
	flushBullets()
	flush()
	return sb.String()
}

// wholeLineBold reports a line wrapped entirely in ** ** and returns its inside.
func wholeLineBold(line string) (string, bool) {
	if len(line) > 4 && strings.HasPrefix(line, "**") && strings.HasSuffix(line, "**") &&
		!strings.Contains(line[2:len(line)-2], "**") {
		return line[2 : len(line)-2], true
	}
	return "", false
}

// inlineBold converts paired ** ** inside a line. An unpaired ** is left as the
// text it is, because the alternative is a dangling <strong> that swallows the
// rest of the document.
func inlineBold(text string) string {
	parts := strings.Split(text, "**")
	if len(parts) < 3 || len(parts)%2 == 0 {
		return text
	}
	var sb strings.Builder
	for i, part := range parts {
		if i%2 == 1 {
			sb.WriteString("<strong>" + part + "</strong>")
			continue
		}
		sb.WriteString(part)
	}
	return sb.String()
}

// htmlAttrEscape escapes a value for safe inclusion in an HTML attribute.
// Recording ids/room names are server-side, but escape defensively.
func htmlAttrEscape(s string) string {
	r := strings.NewReplacer(`&`, "&amp;", `"`, "&quot;", `'`, "&#39;", `<`, "&lt;", `>`, "&gt;")
	return r.Replace(s)
}

// recapToHTML wraps the recap markdown-ish text into the simple paragraph
// HTML the post/chat pipeline expects. Newlines become paragraph breaks;
// the text is HTML-escaped first to prevent injection. When playAttrs is
// non-empty, a recordingEmbed block is appended; the FE renders it as a
// "▶ Play recording" button that opens the player in place, so the recap
// doubles as a one-click way to rewatch the call. notesDocUUID, when set, adds
// a link to the editable notes document.
func recapToHTML(recap, playAttrs, notesDocUUID, gapNote string) string {
	var sb strings.Builder
	sb.WriteString("<p><strong>🤖 Meeting Recap</strong></p>")
	sb.WriteString(modelTextToHTML(recap))
	if attrs := strings.TrimSpace(playAttrs); attrs != "" {
		sb.WriteString(fmt.Sprintf(`<div data-type="recording-embed"%s></div>`, playAttrs))
	}
	// The notes document, if one was written. Without this the document exists
	// in the docs list with no path from the call it came from, which is a
	// document nobody finds. The message is the notification; the document is
	// the thing it points at.
	if note := strings.TrimSpace(gapNote); note != "" {
		sb.WriteString("<p><em>" + html.EscapeString(note) + "</em></p>")
	}
	if id := strings.TrimSpace(notesDocUUID); id != "" {
		sb.WriteString(fmt.Sprintf(
			`<p><a href="/app/doc/%s">📄 Open the meeting notes</a></p>`, htmlAttrEscape(id)))
	}
	return MarkAIGenerated(sb.String())
}

// --- Reading a past call ------------------------------------------------------

// transcriptMaxChars bounds how much of a call we hand back to the model, for the
// same reason readDocMaxChars exists: a long call would otherwise eat the context
// budget and the cost ceiling in one tool call.
const transcriptMaxChars = 12000

// transcriptMaxLines bounds the fetch itself, so a marathon call does not pull
// thousands of rows out of the store to then throw most of them away.
const transcriptMaxLines = 600

// executeReadMeetingTranscript returns what was said in the most recent call in a
// channel.
//
// WHY THIS EXISTS. The recap agent already turns a finished call into decisions
// and action items and posts them, which is the differentiated thing OneCamp can
// do because it owns the call, the transcript and the destination in one system.
// But the recap is a message and the transcript behind it was unreachable, so an
// agent asked "what did we actually agree" could only re-read a summary somebody
// else's model wrote. The detail was there and nothing could get at it.
//
// PERMISSION IS THE WHOLE DESIGN. GetTranscriptLinesByRoom takes a room name and
// no user: it is an internal call made by the recap agent, which has already
// earned its context. Exposed to an agent it becomes "read any call in the
// workspace", and a call transcript is the most sensitive content this product
// holds. So access is decided here against the same accessible-channel set the
// channel summary uses, before the transcript store is touched at all.
//
// Read-only. It cannot start, stop or alter a call.
func executeReadMeetingTranscript(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	channelUUID := strings.TrimSpace(action.Params["channel_uuid"])
	if channelUUID == "" {
		return "", nil, fmt.Errorf("channel_uuid is required")
	}
	if _, err := uuid.Parse(channelUUID); err != nil {
		return "", nil, fmt.Errorf("invalid channel id")
	}

	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to look up user: %w", err)
	}

	// The gate. Same source of truth as the channel summary, so a change to what
	// a person can see reaches both without either being told.
	accessibleChannels, _ := getAccessibleResourceUUIDs(userInfo)
	permitted := false
	for _, c := range accessibleChannels {
		if c == channelUUID {
			permitted = true
			break
		}
	}
	if !permitted {
		// Same answer as a channel that does not exist. Telling somebody a call
		// happened somewhere they cannot see is itself a disclosure.
		return "", nil, fmt.Errorf("no call found for that channel, or you do not have access to it")
	}

	// The room name for a channel call IS the channel id (see deliverRecap).
	lines, _, err := transcriptDomain.GetTranscriptLinesByRoom(ctx, channelUUID, transcriptMaxLines)
	if err != nil {
		return "", nil, fmt.Errorf("could not read the call transcript: %w", err)
	}
	if len(lines) == 0 {
		return "No call transcript found for this channel. Either no call has happened here, or transcription was off.", nil, nil
	}

	// Reuse the recap agent's own formatting rather than growing a second one:
	// speaker names resolved the same way, lines rendered the same way, so a
	// transcript read by an agent looks like the transcript the recap was made
	// from.
	nameByUID := resolveParticipantNames(ctx, lines)
	text, _ := formatTranscriptForRecap(lines, nameByUID)
	text = strings.TrimSpace(text)
	if text == "" {
		return "The call has a transcript but no speech was captured in it.", nil, nil
	}
	truncated := false
	if len(text) > transcriptMaxChars {
		text = text[:transcriptMaxChars]
		truncated = true
	}

	var b strings.Builder
	b.WriteString("Transcript of the most recent call in this channel:\n\n")
	b.WriteString(text)
	if truncated {
		b.WriteString("\n\n(truncated: the call was longer than can be read in one go)")
	}
	return b.String(), map[string]string{"channel_uuid": channelUUID}, nil
}
