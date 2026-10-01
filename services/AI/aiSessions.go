package ai

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

const (
	// SessionTTL is the canonical TTL for AI conversation sessions.
	// Mirrored on registry.AISession.TTL; kept as an exported constant
	// because callers (test fixtures, business code) read it directly.
	SessionTTL = 30 * time.Minute

	// MaxSessionMessages is the maximum number of messages (user+assistant pairs)
	// to keep in a session. Older messages are trimmed to control token usage.
	// 10 pairs = 20 messages = approx 6000 tokens of history
	MaxSessionMessages = 20

	// MaxMessageLength caps individual message length stored in session
	// to prevent token overflow from very long AI responses.
	MaxMessageLength = 2000
)

// ConversationSession holds the message history for a user's AI conversation.
type ConversationSession struct {
	SessionID string        `json:"session_id"`
	UserUUID  string        `json:"user_uuid"`
	Messages  []ChatMessage `json:"messages"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

// CreateSession creates a new conversation session and returns its ID.
// Backed by registry.AISession; gracefully no-ops when Redis is unavailable.
func CreateSession(ctx context.Context, userUUID string) (string, error) {
	if !redisStore.IsAvailable() {
		return "", nil // Graceful no-op
	}

	sessionID := uuid.New().String()
	session := &ConversationSession{
		SessionID: sessionID,
		UserUUID:  userUUID,
		Messages:  []ChatMessage{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := saveSession(ctx, session); err != nil {
		return "", err
	}

	return sessionID, nil
}

// GetSession retrieves a session from Redis. Returns nil if not found or expired.
func GetSession(ctx context.Context, sessionID string) (*ConversationSession, error) {
	var session ConversationSession
	hit, err := redisStore.GetJSON(ctx, registry.AISession, []string{sessionID}, &session)
	if err != nil {
		return nil, err
	}
	if !hit {
		return nil, nil
	}
	return &session, nil
}

// AppendToSession adds a user message and assistant response to the session.
// Automatically trims old messages to stay within MaxSessionMessages.
//
// ownerUUID is who the conversation belongs to. It is required rather than
// optional: the recovery branch below used to leave the owner blank, and a
// blank owner is exactly what GetSessionHistory treats as "nothing to check",
// so a recovered session was readable by anyone holding its id. That branch is
// now the ordinary first turn of every conversation, which makes stamping the
// owner here the difference between a check and the appearance of one.
func AppendToSession(ctx context.Context, sessionID, ownerUUID string, userMsg, assistantMsg string) error {
	session, err := GetSession(ctx, sessionID)
	if err != nil {
		return err
	}

	// If session doesn't exist, create a new one (graceful recovery)
	if session == nil {
		session = &ConversationSession{
			SessionID: sessionID,
			UserUUID:  ownerUUID,
			Messages:  []ChatMessage{},
			CreatedAt: time.Now(),
		}
	}

	// Never let an append hand a session to a different person, and fill in an
	// owner that an older record was written without.
	if session.UserUUID == "" {
		session.UserUUID = ownerUUID
	} else if !sessionBelongsTo(session.UserUUID, ownerUUID) {
		helpers.MessageLogs.ErrorLog.Printf("AI session append rejected: session %s belongs to another user", sessionID)
		return nil
	}

	// Truncate long messages to control token usage
	userMsg = truncateMessage(userMsg, MaxMessageLength)
	assistantMsg = truncateMessage(assistantMsg, MaxMessageLength)

	// Append new messages
	session.Messages = append(session.Messages,
		ChatMessage{Role: "user", Content: userMsg},
		ChatMessage{Role: "assistant", Content: assistantMsg},
	)

	// Trim to keep only the last MaxSessionMessages messages
	if len(session.Messages) > MaxSessionMessages {
		session.Messages = session.Messages[len(session.Messages)-MaxSessionMessages:]
	}

	session.UpdatedAt = time.Now()

	return saveSession(ctx, session)
}

// sessionBelongsTo reports whether caller may see or extend the conversation
// stored under owner.
//
// The subtlety is the blank stored owner. It used to mean "no owner recorded,
// so skip the check", which reads as a check and behaves as its absence: any
// id holder passed it. Every append now stamps an owner, so a blank one can
// only come from a record written before that, and the strict answer costs
// such a record at most its remaining half hour of working memory.
func sessionBelongsTo(owner, caller string) bool {
	if caller == "" {
		return false
	}
	return owner == caller
}

// GetSessionHistory returns the conversation history as ChatMessages
// suitable for prepending to an LLM request.
// Validates session ownership to prevent cross-user session access.
func GetSessionHistory(ctx context.Context, sessionID string, userUUID ...string) ([]ChatMessage, error) {
	if sessionID == "" {
		return nil, nil
	}

	session, err := GetSession(ctx, sessionID)
	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf("AI session lookup failed for %s: %v", sessionID, err)
		return nil, nil // Don't fail the request if session lookup fails
	}

	if session == nil {
		return nil, nil
	}

	// Validate session ownership if userUUID is provided
	if len(userUUID) > 0 && !sessionBelongsTo(session.UserUUID, userUUID[0]) {
		helpers.MessageLogs.ErrorLog.Printf("AI session ownership mismatch: session %s not readable by caller", sessionID)
		return nil, nil // Silently reject, don't expose that the session exists
	}

	return session.Messages, nil
}

// saveSession writes the session to Redis with TTL from the registry.
func saveSession(ctx context.Context, session *ConversationSession) error {
	return redisStore.SetJSON(ctx, registry.AISession, []string{session.SessionID}, session)
}

// truncateMessage caps a message at maxLen runes (not bytes) to avoid
// splitting multibyte UTF-8 characters.
func truncateMessage(msg string, maxLen int) string {
	runes := []rune(msg)
	if len(runes) <= maxLen {
		return msg
	}
	return string(runes[:maxLen]) + "..."
}
