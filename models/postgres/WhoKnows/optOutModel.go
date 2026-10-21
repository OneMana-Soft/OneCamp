// Package models keeps who has asked to be left out of "Who knows" (DG9):
// one row a person, none for everyone else (in by default, O-DG3).
package models

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/akashc777/OneCamp/models/pgexec"
)

// OptedOut is which of ids (person uuids) asked to be left out, by lower-case
// uuid. Ids that aren't uuids are skipped; none asked is an empty map.
func OptedOut(ctx context.Context, ids []string) (map[string]bool, error) {
	var asked []string
	for _, id := range ids {
		if u, err := uuid.Parse(id); err == nil {
			asked = append(asked, u.String())
		}
	}
	out := map[string]bool{}
	if len(asked) == 0 {
		return out, nil
	}
	got, err := pgexec.Query(ctx, `SELECT user_uuid::text FROM who_knows_opt_outs WHERE user_uuid = ANY($1::uuid[])`,
		func(r pgexec.Row) (string, error) {
			var s string
			err := r.Scan(&s)
			return s, err
		}, pq.Array(asked))
	if err != nil {
		return nil, err
	}
	for _, s := range got {
		out[strings.ToLower(s)] = true
	}
	return out, nil
}

// Listed is whether a person may be suggested: true unless they asked not to.
func Listed(ctx context.Context, person uuid.UUID) (bool, error) {
	out, err := OptedOut(ctx, []string{person.String()})
	if err != nil {
		return false, err
	}
	return !out[person.String()], nil
}

// SetListed puts a person back (listed) or takes them out of every answer.
// Idempotent both ways.
func SetListed(ctx context.Context, person uuid.UUID, listed bool) error {
	if listed {
		_, err := pgexec.Exec(ctx, `DELETE FROM who_knows_opt_outs WHERE user_uuid = $1`, person)
		return err
	}
	_, err := pgexec.Exec(ctx, `INSERT INTO who_knows_opt_outs (user_uuid) VALUES ($1) ON CONFLICT (user_uuid) DO NOTHING`, person)
	return err
}
