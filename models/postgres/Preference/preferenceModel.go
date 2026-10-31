// Package Preference is the person_preferences table (design #37): one row
// for each value a person has set, by key; a key with no row is its default.
package Preference

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// kept is one row's key or person, and its value.
type kept[K any] struct {
	of    K
	value json.RawMessage
}

func scanKept[K any](r pgexec.Row) (kept[K], error) {
	var k kept[K]
	var value []byte
	err := r.Scan(&k.of, &value)
	k.value = value
	return k, err
}

// All is every value this person has set, by key.
func All(ctx context.Context, user uuid.UUID) (map[string]json.RawMessage, error) {
	rows, err := pgexec.Query(ctx, `SELECT key, value FROM person_preferences WHERE user_id = $1`, scanKept[string], user)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Preference.All err: %+v", err)
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(rows))
	for _, k := range rows {
		out[k.of] = k.value
	}
	return out, nil
}

// OfPeople is one key's value for each of these people who set it, in one query.
func OfPeople(ctx context.Context, key string, people []uuid.UUID) (map[uuid.UUID]json.RawMessage, error) {
	out := map[uuid.UUID]json.RawMessage{}
	if len(people) == 0 {
		return out, nil
	}
	ids := make([]string, len(people))
	for i, p := range people {
		ids[i] = p.String()
	}
	rows, err := pgexec.Query(ctx, `SELECT user_id, value FROM person_preferences WHERE key = $1 AND user_id = ANY($2::uuid[])`, scanKept[uuid.UUID], key, pq.Array(ids))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Preference.OfPeople err: %+v", err)
		return nil, err
	}
	for _, k := range rows {
		out[k.of] = k.value
	}
	return out, nil
}

// Put sets one value, as the person's own.
func Put(ctx context.Context, user uuid.UUID, key string, value json.RawMessage) error {
	_, err := pgexec.Exec(ctx, `INSERT INTO person_preferences (user_id, key, value, updated_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (user_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, user, key, []byte(value))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Preference.Put err: %+v", err)
	}
	return err
}

// Clear takes one value away, so the key reads as its default again.
func Clear(ctx context.Context, user uuid.UUID, key string) error {
	_, err := pgexec.Exec(ctx, `DELETE FROM person_preferences WHERE user_id = $1 AND key = $2`, user, key)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Preference.Clear err: %+v", err)
	}
	return err
}
