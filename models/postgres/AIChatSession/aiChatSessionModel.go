package models

// Durable AI conversation sessions (migration 153).
//
// Redis remains the hot path for the working history a prompt is built from,
// where a short TTL and a tight message cap are the right tools. This is the
// record beside it, answering a different question: not "what should go in the
// next prompt" but "what did we talk about, and can I get back to it".

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// ChatSession is one conversation.
type ChatSession struct {
	Id        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// MessageCount lets the list show weight without loading any content.
	MessageCount int `json:"message_count"`
}

// ChatMessage is one turn.
type ChatMessage struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// EnsureSession creates the session row if it does not exist.
//
// Takes the id rather than generating one, because the id is minted where the
// conversation starts and Redis already knows it. Two stores holding different
// ids for one conversation would be worse than no durable store at all.
func EnsureSession(ctx context.Context, sessionID, userID uuid.UUID, title string) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `INSERT INTO ai_chat_sessions (id, user_id, title)
	           VALUES ($1, $2, $3)
	           ON CONFLICT (id) DO NOTHING`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, sessionID, userID, title); err != nil {
		helpers.LogErrorWithContext(ctx, "models/EnsureSession err: %+v", err)
		return err
	}
	return nil
}

// AppendMessages records one exchange and moves the session to the top of the
// list. Both writes in one transaction: a message with no bump would sink a live
// conversation down the list, and a bump with no message would promote an empty
// one.
func AppendMessages(ctx context.Context, sessionID uuid.UUID, userMsg, assistantMsg string) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	const insert = `INSERT INTO ai_chat_messages (session_id, role, content) VALUES ($1, $2, $3)`
	if _, err := tx.ExecContext(dbctx, insert, sessionID, "user", userMsg); err != nil {
		helpers.LogErrorWithContext(ctx, "models/AppendMessages user err: %+v", err)
		return err
	}
	if _, err := tx.ExecContext(dbctx, insert, sessionID, "assistant", assistantMsg); err != nil {
		helpers.LogErrorWithContext(ctx, "models/AppendMessages assistant err: %+v", err)
		return err
	}
	if _, err := tx.ExecContext(dbctx,
		`UPDATE ai_chat_sessions SET updated_at = NOW() WHERE id = $1`, sessionID); err != nil {
		helpers.LogErrorWithContext(ctx, "models/AppendMessages touch err: %+v", err)
		return err
	}
	return tx.Commit()
}

// ListSessions returns a user's conversations, most recently active first.
//
// Scoped by user_id in the query rather than checked afterwards: a conversation
// is the most personal thing in the product, and an ownership check that lives
// anywhere but the WHERE clause is one refactor away from not happening.
func ListSessions(ctx context.Context, userID uuid.UUID, limit int) ([]*ChatSession, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const q = `SELECT s.id, s.title, s.created_at, s.updated_at,
	                  (SELECT COUNT(*) FROM ai_chat_messages m WHERE m.session_id = s.id) AS message_count
	           FROM ai_chat_sessions s
	           WHERE s.user_id = $1 AND s.deleted_at IS NULL
	           ORDER BY s.updated_at DESC
	           LIMIT $2`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, userID, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListSessions err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	out := []*ChatSession{}
	for rows.Next() {
		var s ChatSession
		if err := rows.Scan(&s.Id, &s.Title, &s.CreatedAt, &s.UpdatedAt, &s.MessageCount); err != nil {
			return nil, err
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}

// GetSessionMessages replays one conversation, oldest first.
//
// Also scoped by user_id, through the join. Reading somebody else's conversation
// by guessing a uuid must not be possible, and the only reliable way to
// guarantee that is to make the query itself incapable of returning one.
func GetSessionMessages(ctx context.Context, sessionID, userID uuid.UUID) ([]*ChatMessage, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT m.role, m.content, m.created_at
	           FROM ai_chat_messages m
	           JOIN ai_chat_sessions s ON s.id = m.session_id
	           WHERE m.session_id = $1 AND s.user_id = $2 AND s.deleted_at IS NULL
	           ORDER BY m.created_at ASC`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, sessionID, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetSessionMessages err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	out := []*ChatMessage{}
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// SoftDeleteSession removes a conversation from the list.
//
// Soft, and scoped by owner in the same statement, so a delete cannot reach a
// conversation the caller does not own even if the id is right.
func SoftDeleteSession(ctx context.Context, sessionID, userID uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`UPDATE ai_chat_sessions SET deleted_at = NOW() WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`,
		sessionID, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SoftDeleteSession err: %+v", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
