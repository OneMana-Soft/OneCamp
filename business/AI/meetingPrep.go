package business

// Pre-meeting prep brief — calendar intelligence.
//
// For an upcoming calendar event, MeetingPrepBrief turns the meeting's own
// topic (title + description + attendees) into a short, actionable brief so
// people walk in prepared: what the meeting is about, suggested talking
// points, relevant recent discussion, and the requester's open commitments
// that might be worth raising.
//
// This is the read-only, on-demand half of OneCamp's meeting intelligence
// (the ambient post-call recap is the push half). It is the kind of
// workspace-aware help a chat-only or calendar-only tool can't do: it joins
// the calendar event, the semantic index of workspace discussion, AND the
// structured memory layer in one system.
//
// Design / safety:
//   - Gated on AI being enabled (provider-agnostic: the active model behind
//     the AI service chokepoint produces the brief; no vendor coupling).
//   - Permission-correct: only the event's creator or a participant may ask,
//     and the gathered context (semantic recall + open memory items) reuses
//     the exact same permission filters as AskAI/briefing, so the brief is
//     grounded only in content the requester can already see.
//   - Grounded: the related discussion + open items are passed verbatim; the
//     model is told to use only what it's given and never invent.
//   - Bounded: snippet counts and prompt size are capped; the LLM call goes
//     through the same circuit breaker as the rest of AI.

import (
	"context"
	"fmt"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	calendarDomain "github.com/akashc777/OneCamp/domain/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	prepMaxRelated     = 6    // related discussion snippets fed to the model
	prepMaxOpenItems   = 6    // requester's open commitments/questions
	prepMaxSnippetLen  = 280  // per-snippet char cap (keeps the prompt lean)
	prepMaxPromptChars = 8000 // hard cap on the assembled context block
)

// prepSystemPrompt steers the model toward a tight, factual prep brief.
const prepSystemPrompt = `You are OneCamp's meeting assistant. You are given an upcoming meeting (title, time, attendees, optional description) and, when available, RELATED workspace discussion and the organizer's OPEN follow-up items.
Write a concise pre-meeting brief with EXACTLY these sections, in this order, using markdown:

**🎯 Purpose**
- 1-2 bullets: what this meeting is for, inferred from its title/description.

**🗣️ Suggested talking points**
- 2-4 bullets of concrete things worth covering. Ground them in the related discussion when provided; otherwise base them on the title/description.

**📌 Relevant context**
- Brief notes drawn ONLY from the provided related discussion. If none was provided, write "- None found".

**📋 Open items to raise**
- Pull from the provided open follow-up items if any are relevant; otherwise write "- None".

Rules:
- Use ONLY the information provided. Never invent names, dates, decisions, or links.
- Keep attendee and proper-noun names as-is.
- Be concise. No preamble, no closing remarks.
- If there is too little to say, reply with exactly: SKIP_BRIEF`

// MeetingPrepBrief produces an on-demand prep brief for an upcoming event.
// Read-only. Visible to the event's creator or any participant.
func MeetingPrepBrief(ctx context.Context, userInfo *userModels.UserInfo, req adapter.MeetingPrepRequest) (*adapter.MeetingPrepResponse, error) {
	resp := &adapter.MeetingPrepResponse{
		Enabled:   false,
		EventUUID: strings.TrimSpace(req.EventUUID),
	}

	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return resp, nil // AI off → caller hides the surface
	}
	resp.Enabled = true

	eventUUID := strings.TrimSpace(req.EventUUID)
	if eventUUID == "" {
		return nil, fmt.Errorf("event_uuid is required")
	}
	event, err := calendarDomain.GetDgraphEventInfoByUUID(ctx, eventUUID)
	if err != nil || event == nil {
		return nil, fmt.Errorf("event not found")
	}

	// Authorization: only people on the meeting may request its brief.
	me := userInfo.UserDgraphInfo.Uuid
	allowed := event.CreatedBy != nil && event.CreatedBy.Uuid == me
	for _, p := range event.Participants {
		if p != nil && p.Uuid == me {
			allowed = true
		}
	}
	if !allowed {
		return nil, fmt.Errorf("you are not on this event")
	}

	title := strings.TrimSpace(event.Title)
	resp.Title = title

	// Attendee display names (creator + participants), de-duplicated.
	var attendees []string
	seen := map[string]bool{}
	addName := func(uuid, name string) {
		if uuid == "" || seen[uuid] {
			return
		}
		seen[uuid] = true
		if n := strings.TrimSpace(name); n != "" {
			attendees = append(attendees, n)
		}
	}
	if event.CreatedBy != nil {
		addName(event.CreatedBy.Uuid, displayName(event.CreatedBy.UserName, event.CreatedBy.UserFullName))
	}
	for _, p := range event.Participants {
		if p != nil {
			addName(p.Uuid, displayName(p.UserName, p.UserFullName))
		}
	}

	when := ""
	if event.StartTime != nil {
		when = event.StartTime.UTC().Format(time.RFC3339)
	}

	// Gather RELATED workspace discussion by semantic search on the meeting
	// topic, permission-scoped exactly like AskAI recall. Best-effort.
	channels, projects := getAccessibleResourceUUIDs(userInfo)
	grpIDs := accessibleGroupingIDs(userInfo)
	query := strings.TrimSpace(title + " " + helpers.HTMLToPlainText(strings.TrimSpace(event.Description)))
	var related []ai.SimilarResult
	if query != "" {
		if res, serr := ai.SearchSimilar(ctx, query, me, channels, projects, grpIDs, prepMaxRelated); serr == nil {
			related = res
		} else {
			helpers.LogInfoWithContext(ctx, "meeting prep: related search failed: %v", serr)
		}
	}

	// The requester's OPEN follow-up items (owner-scoped). Best-effort.
	ownerID := userInfo.UserPostgresInfo.Id
	openItems, oerr := memoryModels.List(ctx, memoryModels.QueryFilter{
		Statuses: []string{memoryModels.StatusOpen},
		Kinds:    []string{memoryModels.KindCommitment, memoryModels.KindQuestion},
		OwnerID:  &ownerID,
		Limit:    prepMaxOpenItems,
	})
	if oerr != nil {
		helpers.LogInfoWithContext(ctx, "meeting prep: open items failed: %v", oerr)
	}

	contextBlock := buildPrepContext(title, when, attendees, helpers.HTMLToPlainText(strings.TrimSpace(event.Description)), related, openItems)

	// Summarize through the circuit breaker.
	if cbErr := svc.Resiliency.CB.Allow(); cbErr != nil {
		resp.Note = "The assistant is busy right now. Try again in a moment."
		return resp, nil
	}
	brief, serr := svc.Summarize(ctx, contextBlock, prepSystemPrompt)
	if serr != nil {
		svc.Resiliency.CB.RecordResult(serr)
		resp.Note = "Couldn't generate a brief right now."
		return resp, nil
	}
	svc.Resiliency.CB.RecordSuccess()

	brief = strings.TrimSpace(brief)
	if brief == "" || strings.Contains(brief, "SKIP_BRIEF") {
		resp.Note = "Not enough context yet to prepare a brief for this meeting."
		return resp, nil
	}
	resp.Brief = SanitizeResponse(brief)
	if resp.Brief == "" {
		resp.Note = "Not enough context yet to prepare a brief for this meeting."
	}
	return resp, nil
}

// buildPrepContext assembles the bounded, plain-text context block handed to
// the model: the meeting facts, then related discussion snippets, then the
// requester's open items. Pure + deterministic (DB-free) so it can be unit
// tested. Snippets and the whole block are length-capped to keep prompt size
// and cost bounded.
func buildPrepContext(title, when string, attendees []string, description string, related []ai.SimilarResult, openItems []*memoryModels.MemoryItem) string {
	var sb strings.Builder

	sb.WriteString("MEETING\n")
	if title != "" {
		sb.WriteString("Title: " + title + "\n")
	}
	if when != "" {
		sb.WriteString("When: " + when + "\n")
	}
	if len(attendees) > 0 {
		sb.WriteString("Attendees: " + strings.Join(attendees, ", ") + "\n")
	}
	if d := strings.TrimSpace(description); d != "" {
		sb.WriteString("Description: " + clipPrepText(d, prepMaxSnippetLen*2) + "\n")
	}

	if len(related) > 0 {
		sb.WriteString("\nRELATED DISCUSSION\n")
		for _, r := range related {
			text := clipPrepText(strings.TrimSpace(helpers.HTMLToPlainText(r.ContentText)), prepMaxSnippetLen)
			if text == "" {
				continue
			}
			where := strings.TrimSpace(r.ChannelName)
			who := strings.TrimSpace(r.AuthorName)
			prefix := "-"
			switch {
			case where != "" && who != "":
				prefix = fmt.Sprintf("- [%s, %s]", where, who)
			case where != "":
				prefix = fmt.Sprintf("- [%s]", where)
			case who != "":
				prefix = fmt.Sprintf("- [%s]", who)
			}
			line := prefix + " " + text + "\n"
			if sb.Len()+len(line) > prepMaxPromptChars {
				break
			}
			sb.WriteString(line)
		}
	}

	if len(openItems) > 0 {
		sb.WriteString("\nORGANIZER OPEN ITEMS\n")
		for _, it := range openItems {
			if it == nil {
				continue
			}
			c := clipPrepText(strings.TrimSpace(it.Content), prepMaxSnippetLen)
			if c == "" {
				continue
			}
			line := "- (" + it.Kind + ") " + c + "\n"
			if sb.Len()+len(line) > prepMaxPromptChars {
				break
			}
			sb.WriteString(line)
		}
	}

	return sb.String()
}

// clipPrepText trims s to at most max runes, adding an ellipsis when cut.
func clipPrepText(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
