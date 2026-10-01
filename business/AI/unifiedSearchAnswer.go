package business

// Grounded AI answer over unified search (the Notion-AI / Glean "answer with
// verified sources" surface).
//
// AnswerFromSearch runs the same permission-scoped, multi-source fan-out as
// UnifiedSearch, then asks the model for a SHORT direct answer that cites the
// retrieved hits by bracketed number ([1], [2], …). The answer is grounded:
// the model is instructed to use ONLY the provided sources and to say when they
// don't contain the answer, so there is no ungrounded speculation. Every
// citation the FE renders is a real hit the caller already has access to (the
// same routing fields UnifiedSearch returns), so a citation is click-through to
// the exact post/task/doc/email/PR — never a fabricated reference.
//
// It reuses the AskAI residency + resiliency chokepoint (workspace model, rate
// limiter, circuit breaker, token budget). The heavy lifting (retrieval) is the
// cached UnifiedSearch call, so an answer never re-hits the external APIs when
// the results list was just loaded.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	maxAnswerCitations    = 8
	answerCitationSnippet = 320
)

// searchAnswerSystemPrompt constrains the model to a grounded, cited answer.
const searchAnswerSystemPrompt = `You answer a user's question using ONLY the numbered sources provided. Rules:
- Be direct and concise: 1-4 sentences, no preamble.
- Cite every claim with the bracketed source number it came from, e.g. [1] or [2][3].
- Use only facts present in the sources. Do NOT invent details, names, dates, or numbers.
- If the sources do not contain enough to answer, say so plainly in one line and cite nothing.
- Never output HTML or markdown headings; plain sentences with [n] citations only.`

// SearchCitation is one grounded source behind an answer. Index is the 1-based
// marker used in the answer text ([Index]). The routing fields mirror
// UnifiedHit so the FE reuses its existing deep-link navigation for a citation
// click (external hits carry URL instead).
type SearchCitation struct {
	Index   int    `json:"index"`
	Source  string `json:"source"`
	Title   string `json:"title"`
	Snippet string `json:"snippet,omitempty"`
	Meta    string `json:"meta,omitempty"`
	URL     string `json:"url,omitempty"`
	Kind    string `json:"kind,omitempty"`

	ContentType  string `json:"content_type,omitempty"`
	ContentUUID  string `json:"content_uuid,omitempty"`
	ChannelUUID  string `json:"channel_uuid,omitempty"`
	ChannelName  string `json:"channel_name,omitempty"`
	ProjectUUID  string `json:"project_uuid,omitempty"`
	ChatGrpID    string `json:"chat_grp_id,omitempty"`
	ChatByUserID string `json:"chat_by_user_id,omitempty"`
	ChatToUserID string `json:"chat_to_user_id,omitempty"`
	PostUUID     string `json:"post_uuid,omitempty"`
	TaskUUID     string `json:"task_uuid,omitempty"`
	DocUUID      string `json:"doc_uuid,omitempty"`
}

// UnifiedAnswerResponse is the grounded-answer payload. Enabled=false → AI is
// off (the caller hides the surface). When no sources match, Answer is empty
// and Note explains why, so the FE shows a clean empty state instead of a
// hallucinated answer.
type UnifiedAnswerResponse struct {
	Enabled   bool             `json:"enabled"`
	Query     string           `json:"query"`
	Answer    string           `json:"answer"`
	Citations []SearchCitation `json:"citations"`
	Note      string           `json:"note,omitempty"`
	Provider  string           `json:"provider,omitempty"`
	// Notice is set when the retrieved material was shortened to fit the model's
	// context window, so the reader knows the answer was synthesised over part of what
	// the search found. Kept separate from Note, which explains why there is no answer:
	// one describes a missing result and the other a partial input, and folding them
	// into one field would make both ambiguous.
	Notice string `json:"notice,omitempty"`
}

// AnswerFromSearch produces a grounded, cited answer for a query by synthesizing
// over the caller's unified search results. Permission-correct (retrieval is
// UnifiedSearch, which never widens access) and best-effort: a model failure
// returns an error the controller maps, an empty corpus returns a clean note.
func AnswerFromSearch(ctx context.Context, userInfo *userModels.UserInfo, query string) (*UnifiedAnswerResponse, error) {
	resp := &UnifiedAnswerResponse{Enabled: false, Citations: []SearchCitation{}}

	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return resp, nil // AI off → caller hides the surface
	}
	resp.Enabled = true

	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("a search query is required")
	}
	resp.Query = query

	// Retrieval: reuse the cached, permission-scoped multi-source fan-out.
	search, err := UnifiedSearch(ctx, userInfo, query)
	if err != nil {
		return nil, err
	}
	resp.Query = search.Query

	citations := flattenCitations(search.Groups, maxAnswerCitations)
	if len(citations) == 0 {
		resp.Note = "No relevant sources found for this question."
		return resp, nil
	}

	// Resolve the member's admin-authorized model pick (falling back to the
	// workspace default, honoring residency/local-only) — a search answer is an
	// interactive, user-initiated query, so it behaves like AskAI rather than a
	// background summarization. The picked model carries its OWN circuit breaker.
	userUUID := userInfo.UserDgraphInfo.Uuid
	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	if llm == nil {
		return resp, nil // AI became unavailable between checks — hide the surface
	}
	// Only now, with real sources, spend a call: breaker (of the RESOLVED model)
	// + per-user rate limit. Daily token caps (user/workspace) are enforced and
	// metered at the provider chokepoint inside llm.Chat.
	if err := cb.Allow(); err != nil {
		return nil, err
	}
	if rlErr := svc.Resiliency.CheckRateLimit(ctx, userUUID); rlErr != nil {
		return nil, rlErr
	}
	// Sink attached here rather than in the controller because this function owns the
	// response it would be reported on, so the two cannot drift apart.
	ctx = ai.WithContextNoticeSink(ctx)
	ai.NoteQuestion(ctx, search.Query)
	limits := ai.LimitsFrom(ctx)
	content := limits.TruncateForPrompt(ctx, buildAnswerContext(search.Query, citations), limits.ContextBudget())
	answer, aerr := ai.ChatWithRescue(ctx, llm, []ai.ChatMessage{
		{Role: "system", Content: searchAnswerSystemPrompt},
		{Role: "user", Content: content},
	}, ai.ChatOptions{Temperature: 0.2, MaxTokens: 1024})
	if aerr != nil {
		cb.RecordResult(aerr)
		return nil, fmt.Errorf("answer synthesis failed: %w", aerr)
	}
	cb.RecordSuccess()

	resp.Answer = SanitizeResponse(answer)
	resp.Citations = pruneToReferenced(citations, resp.Answer)
	resp.Provider = string(svc.Config.Provider())
	resp.Notice = ai.TakeContextNotice(ctx).Message()
	return resp, nil
}

// flattenCitations turns the grouped search hits into a flat, 1-based numbered
// citation list in stable group order (workspace, memory, gmail, github),
// capped at max. It carries every routing field forward so a citation click
// deep-links exactly like the corresponding search hit. Pure + unit-tested.
func flattenCitations(groups []UnifiedSearchGroup, max int) []SearchCitation {
	out := make([]SearchCitation, 0, max)
	n := 0
	for _, g := range groups {
		for _, h := range g.Hits {
			if strings.TrimSpace(h.Title) == "" && strings.TrimSpace(h.Snippet) == "" {
				continue
			}
			n++
			out = append(out, SearchCitation{
				Index:        n,
				Source:       h.Source,
				Title:        h.Title,
				Snippet:      clipAnswer(h.Snippet),
				Meta:         h.Meta,
				URL:          h.URL,
				Kind:         h.Kind,
				ContentType:  h.ContentType,
				ContentUUID:  h.ContentUUID,
				ChannelUUID:  h.ChannelUUID,
				ChannelName:  h.ChannelName,
				ProjectUUID:  h.ProjectUUID,
				ChatGrpID:    h.ChatGrpID,
				ChatByUserID: h.ChatByUserID,
				ChatToUserID: h.ChatToUserID,
				PostUUID:     h.PostUUID,
				TaskUUID:     h.TaskUUID,
				DocUUID:      h.DocUUID,
			})
			if n >= max {
				return out
			}
		}
	}
	return out
}

// buildAnswerContext renders the numbered sources into the grounding block the
// model reads. Each source shows its number, group label, title, meta, and
// snippet. Pure + unit-tested.
func buildAnswerContext(query string, citations []SearchCitation) string {
	var b strings.Builder
	b.WriteString("Question: ")
	b.WriteString(query)
	b.WriteString("\n\nSources:\n")
	for _, c := range citations {
		b.WriteString("[")
		b.WriteString(strconv.Itoa(c.Index))
		b.WriteString("] (")
		b.WriteString(sourceLabel(c.Source))
		b.WriteString(") ")
		if t := strings.TrimSpace(c.Title); t != "" {
			b.WriteString(t)
		}
		if m := strings.TrimSpace(c.Meta); m != "" {
			b.WriteString(" — ")
			b.WriteString(m)
		}
		if s := strings.TrimSpace(c.Snippet); s != "" {
			b.WriteString("\n    ")
			b.WriteString(s)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nAnswer the question using only these sources, citing with [n].")
	return b.String()
}

// sourceLabel maps a source id to a short human label for the grounding block.
// Pure.
func sourceLabel(source string) string {
	switch source {
	case UnifiedSourceWorkspace:
		return "Workspace"
	case UnifiedSourceMemory:
		return "Memory"
	case UnifiedSourceGmail:
		return "Gmail"
	case UnifiedSourceGitHub:
		return "GitHub"
	default:
		return "Source"
	}
}

// pruneToReferenced keeps only the citations whose [n] marker actually appears
// in the answer, preserving order. If the model cited nothing recognizable
// (e.g. a "not enough info" answer), the full list is returned unchanged so the
// FE can still show what was searched. Pure + unit-tested.
func pruneToReferenced(citations []SearchCitation, answer string) []SearchCitation {
	refs := referencedIndexes(answer)
	if len(refs) == 0 {
		return citations
	}
	out := make([]SearchCitation, 0, len(citations))
	for _, c := range citations {
		if refs[c.Index] {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return citations
	}
	return out
}

// referencedIndexes extracts the set of 1-based numbers cited as [n] in text.
// Pure. Tolerates multi-citation forms like [1][2] and [1, 2].
func referencedIndexes(text string) map[int]bool {
	refs := map[int]bool{}
	inBracket := false
	num := 0
	has := false
	flush := func() {
		if has && num > 0 {
			refs[num] = true
		}
		num = 0
		has = false
	}
	for _, r := range text {
		switch {
		case r == '[':
			inBracket = true
			num = 0
			has = false
		case r == ']':
			if inBracket {
				flush()
			}
			inBracket = false
		case inBracket && r >= '0' && r <= '9':
			num = num*10 + int(r-'0')
			has = true
		case inBracket && (r == ',' || r == ' '):
			flush()
		case inBracket:
			// Non-numeric junk inside brackets: reset this token.
			num = 0
			has = false
		}
	}
	return refs
}

// clipAnswer normalizes whitespace and caps a citation snippet. Pure.
func clipAnswer(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	r := []rune(s)
	if len(r) <= answerCitationSnippet {
		return s
	}
	return string(r[:answerCitationSnippet]) + "…"
}
