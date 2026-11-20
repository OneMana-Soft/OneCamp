// Package models (InboxMark) stores a person's own marks on their Inbox items
// (migration 599): today, which items they took out of Priority. Every query
// is scoped to the person: nobody reads or changes another's marks, and no
// row is ever deleted (putting an item back clears its dismissal).
package models

import (
	"context"
	"slices"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// SetDismissed takes the person's keys out of their Priority (dismissed) or
// puts them back: one write. A dismissal keeps the first time it was made; a
// key put back keeps its row with no dismissal, and one never dismissed
// writes nothing.
//
// Both writes lock the person's rows in one order, the keys sorted (and each
// once), so calls at once naming the same items in different orders (tabs,
// a select-many and its Undo) wait for each other instead of deadlocking.
func SetDismissed(ctx context.Context, userID uuid.UUID, keys []string, dismissed bool) error {
	keys = slices.Compact(slices.Sorted(slices.Values(keys)))
	q := `
		INSERT INTO inbox_marks (user_id, item_key, dismissed_at)
		SELECT $1, k, NOW() FROM unnest($2::text[]) AS k
		ON CONFLICT (user_id, item_key) DO UPDATE SET
			dismissed_at = COALESCE(inbox_marks.dismissed_at, EXCLUDED.dismissed_at), updated_at = NOW()`
	if !dismissed {
		q = `
		WITH locked AS (
			SELECT m.item_key FROM unnest($2::text[]) WITH ORDINALITY AS keys(k, n)
			JOIN inbox_marks m ON m.user_id = $1 AND m.item_key = keys.k
			WHERE m.dismissed_at IS NOT NULL
			ORDER BY keys.n FOR UPDATE OF m)
		UPDATE inbox_marks SET dismissed_at = NULL, updated_at = NOW()
		WHERE user_id = $1 AND item_key IN (SELECT item_key FROM locked)`
	}
	_, err := pgexec.Exec(ctx, q, userID, pq.Array(keys))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/InboxMark SetDismissed err: %+v", err)
	}
	return err
}

// Dismissed is which of keys the person has taken out of their Priority: one
// read for a page of their feed.
func Dismissed(ctx context.Context, userID uuid.UUID, keys []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(keys) == 0 {
		return out, nil
	}
	got, err := pgexec.Query(ctx, `
		SELECT item_key FROM inbox_marks
		WHERE user_id = $1 AND item_key = ANY($2::text[]) AND dismissed_at IS NOT NULL`, pgexec.Text, userID, pq.Array(keys))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/InboxMark Dismissed err: %+v", err)
		return nil, err
	}
	for _, k := range got {
		out[k] = true
	}
	return out, nil
}

// PutAllBack puts every item the person took out of Priority back, keeping
// the rows: the demo's reset, so each visitor finds the seeded ask in
// Priority whatever an earlier visitor dismissed.
func PutAllBack(ctx context.Context, userID uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `
		UPDATE inbox_marks SET dismissed_at = NULL, updated_at = NOW()
		WHERE user_id = $1 AND dismissed_at IS NOT NULL`, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/InboxMark PutAllBack err: %+v", err)
	}
	return err
}
