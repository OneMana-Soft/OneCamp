package business

// Durable AI conversations: keeping them, listing them, resuming them.
//
// Redis holds the working history a prompt is assembled from. This holds the
// conversation, and the difference matters: Redis is sized for token control
// with a 30 minute TTL and a twenty message cap, which is correct for its job
// and exactly wrong for remembering what somebody asked yesterday.

import (
	"context"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIChatSession"
)

// maxTitleLen keeps a derived title to one readable line in a sidebar.
const maxTitleLen = 60

// TitleFrom derives a conversation's name from its opening question.
//
// Derived rather than asked for, because nobody names a conversation before
// having it, and a list of "New chat" is a list of nothing. Takes the first
// sentence or clause, which is almost always the actual subject.
func TitleFrom(question string) string {
	q := strings.TrimSpace(question)
	if q == "" {
		return "New conversation"
	}
	// Collapse whitespace so a pasted multi-line prompt does not become a
	// multi-line title.
	q = strings.Join(strings.Fields(q), " ")

	// Stop at the first sentence end, if one arrives early enough to be a title
	// rather than a truncation. A colon counts: it almost always introduces the
	// detail rather than the subject, so "Review this:" is the better title for a
	// question that continues into a pasted list.
	if idx := strings.IndexAny(q, ".?!:"); idx > 8 && idx < maxTitleLen {
		return strings.TrimSpace(q[:idx+1])
	}
	if len(q) <= maxTitleLen {
		return q
	}
	// Cut on a word boundary so the title does not end mid-word.
	cut := q[:maxTitleLen]
	if sp := strings.LastIndexFunc(cut, unicode.IsSpace); sp > 20 {
		cut = cut[:sp]
	}
	return strings.TrimSpace(cut) + "…"
}

// Record persists one exchange, creating the conversation on first use.
//
// Best-effort by contract. The answer has already been streamed to the person by
// the time this runs, so failing to file it must never turn a delivered answer
// into an error they see.
func Record(ctx context.Context, sessionID, userID uuid.UUID, question, answer string) {
	if sessionID == uuid.Nil || userID == uuid.Nil {
		return
	}
	if err := model.EnsureSession(ctx, sessionID, userID, TitleFrom(question)); err != nil {
		helpers.LogErrorWithContext(ctx, "business/chatSession: could not open session %s: %+v", sessionID, err)
		return
	}
	if err := model.AppendMessages(ctx, sessionID, question, answer); err != nil {
		helpers.LogErrorWithContext(ctx, "business/chatSession: could not record exchange in %s: %+v", sessionID, err)
	}
}

// List returns a person's conversations, most recent first.
func List(ctx context.Context, userID uuid.UUID, limit int) ([]*model.ChatSession, error) {
	return model.ListSessions(ctx, userID, limit)
}

// Messages replays one conversation. Ownership is enforced in the query.
func Messages(ctx context.Context, sessionID, userID uuid.UUID) ([]*model.ChatMessage, error) {
	return model.GetSessionMessages(ctx, sessionID, userID)
}

// Delete removes a conversation from a person's list.
func Delete(ctx context.Context, sessionID, userID uuid.UUID) error {
	return model.SoftDeleteSession(ctx, sessionID, userID)
}
