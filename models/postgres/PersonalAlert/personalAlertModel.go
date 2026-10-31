// Package models (PersonalAlert) keeps a person's notification keywords and
// VIPs (migration 245; chat P3 stage 3), through models/pgexec.
package models

import (
	"context"
	"strings"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Keywords is a person's keywords, as they wrote them, in their order.
func Keywords(ctx context.Context, user uuid.UUID) ([]string, error) {
	return list(ctx, `SELECT keyword FROM user_notification_keywords WHERE user_id = $1 ORDER BY position, keyword`, user)
}

// VIPs is a person's VIPs' user ids, in their order.
func VIPs(ctx context.Context, user uuid.UUID) ([]string, error) {
	return list(ctx, `SELECT vip_user_id::text FROM user_vips WHERE user_id = $1 ORDER BY position, vip_user_id`, user)
}

// list is the one text column q reads for user: an empty list, never nil,
// for none.
func list(ctx context.Context, q string, user uuid.UUID) ([]string, error) {
	out, err := pgexec.Query(ctx, q, pgexec.Text, user)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// SetKeywords replaces a person's keywords with keywords, in one transaction,
// each at its place in the list.
func SetKeywords(ctx context.Context, user uuid.UUID, keywords []string) error {
	return replace(ctx, `DELETE FROM user_notification_keywords WHERE user_id = $1`,
		`INSERT INTO user_notification_keywords (user_id, keyword, position) SELECT $1, k, n FROM unnest($2::text[]) WITH ORDINALITY AS t(k, n)`, user, keywords)
}

// SetVIPs replaces a person's VIPs with vips (user ids), in one transaction.
func SetVIPs(ctx context.Context, user uuid.UUID, vips []string) error {
	return replace(ctx, `DELETE FROM user_vips WHERE user_id = $1`,
		`INSERT INTO user_vips (user_id, vip_user_id, position) SELECT $1, v::uuid, n FROM unnest($2::text[]) WITH ORDINALITY AS t(v, n)`, user, vips)
}

// replace runs del, then ins with values, in one transaction.
func replace(ctx context.Context, del, ins string, user uuid.UUID, values []string) error {
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		if _, err := pgexec.Exec(ctx, del, user); err != nil {
			return err
		}
		if len(values) == 0 {
			return nil
		}
		_, err := pgexec.Exec(ctx, ins, user, pq.Array(values))
		return err
	})
}

// KeywordsOf is the keywords of each of the people (user ids) who have any, by
// lower-cased user id: for who a channel message's words reach.
func KeywordsOf(ctx context.Context, users []string) (map[string][]string, error) {
	out := map[string][]string{}
	ids := wellFormed(users)
	if len(ids) == 0 {
		return out, nil
	}
	type keyword struct{ user, word string }
	rows, err := pgexec.Query(ctx,
		`SELECT user_id::text, keyword FROM user_notification_keywords WHERE user_id = ANY($1::uuid[])`,
		func(r pgexec.Row) (k keyword, err error) {
			err = r.Scan(&k.user, &k.word)
			return
		}, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	for _, k := range rows {
		out[k.user] = append(out[k.user], k.word)
	}
	return out, nil
}

// WhoseVIP is those of the people (user ids) who have sender as a VIP, as a
// set of lower-cased user ids.
func WhoseVIP(ctx context.Context, sender string, users []string) (map[string]bool, error) {
	out := map[string]bool{}
	from, err := uuid.Parse(sender)
	ids := wellFormed(users)
	if err != nil || len(ids) == 0 {
		return out, nil
	}
	whose, err := pgexec.Query(ctx,
		`SELECT user_id::text FROM user_vips WHERE vip_user_id = $1 AND user_id = ANY($2::uuid[])`,
		pgexec.Text, from, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	for _, id := range whose {
		out[id] = true
	}
	return out, nil
}

// wellFormed is the uuids of ids, lower-cased, each once; an id that isn't
// one is skipped rather than failing the read for everyone else.
func wellFormed(ids []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		u, err := uuid.Parse(strings.TrimSpace(id))
		if err != nil || seen[u.String()] {
			continue
		}
		seen[u.String()] = true
		out = append(out, u.String())
	}
	return out
}
