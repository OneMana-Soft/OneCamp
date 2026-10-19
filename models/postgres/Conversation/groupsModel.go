package models

// The conversations index as the groups' screens read and change it: one
// conversation by its ref, the people in it a page at a time, the ones a
// person hid, and the channel a group became.

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ByRef is one conversation's row, or nil.
func ByRef(ctx context.Context, ref string) (*Conversation, error) {
	got, err := ByRefs(ctx, []string{ref})
	if err != nil {
		return nil, err
	}
	if c, ok := got[ref]; ok {
		return &c, nil
	}
	return nil, nil
}

// SetContinuedIn records the channel a group became.
func SetContinuedIn(ctx context.Context, id, channelID uuid.UUID) error {
	cctx, cancel := timeout(ctx)
	defer cancel()
	_, err := db().ExecContext(cctx, `UPDATE conversations SET continued_in = $2 WHERE id = $1`, id, channelID)
	return err
}

// SetHidden hides the conversation for a member (at), or shows it again (nil).
func SetHidden(ctx context.Context, conversation, user uuid.UUID, at *time.Time) error {
	cctx, cancel := timeout(ctx)
	defer cancel()
	_, err := db().ExecContext(cctx, `
		UPDATE conversation_members SET hidden_at = $3, updated_at = NOW()
		WHERE conversation_id = $1 AND user_id = $2`, conversation, user, at)
	return err
}

// LiveMembers is a page of a conversation's people, in the order of their ids,
// after after (uuid.Nil for the first page).
func LiveMembers(ctx context.Context, conversation, after uuid.UUID, limit int) ([]Member, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		SELECT conversation_id, user_id, role, joined_at, left_at, hidden_at, updated_at
		FROM conversation_members
		WHERE conversation_id = $1 AND left_at IS NULL AND user_id > $2
		ORDER BY user_id LIMIT $3`, conversation, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// HiddenOf is when a person hid each of their hidden conversations, by ref.
func HiddenOf(ctx context.Context, user uuid.UUID) (map[string]time.Time, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		SELECT c.ref, m.hidden_at FROM conversation_members m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.user_id = $1 AND m.left_at IS NULL AND m.hidden_at IS NOT NULL`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var ref string
		var at time.Time
		if err := rows.Scan(&ref, &at); err != nil {
			return nil, err
		}
		out[ref] = at
	}
	return out, rows.Err()
}
