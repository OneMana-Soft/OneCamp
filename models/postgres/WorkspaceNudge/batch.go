package models

// A person's nudges checked and written a pass at a time (backend audit #36:
// each nudge was its own terminal check and its own upsert).

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// RecentlyTerminal is which of these dedup keys someone dismissed or acted
// on within `within` (HasRecentTerminal for many, in one statement).
func RecentlyTerminal(ctx context.Context, dedupKeys []string, within time.Duration) (map[string]bool, error) {
	out := make(map[string]bool, len(dedupKeys))
	if len(dedupKeys) == 0 {
		return out, nil
	}
	keys, err := pgexec.Query(ctx, `
		SELECT DISTINCT dedup_key FROM workspace_nudges
		WHERE dedup_key = ANY($1) AND status IN ('dismissed','acted') AND updated_at >= $2`,
		func(r pgexec.Row) (k string, err error) { err = r.Scan(&k); return }, pq.Array(dedupKeys), time.Now().Add(-within))
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		out[k] = true
	}
	return out, nil
}

// Upserted is what UpsertMany did for one dedup key.
type Upserted struct {
	DedupKey string
	ID       uuid.UUID
	Inserted bool
}

// UpsertMany is Upsert for one person's nudges in one statement. Each dedup
// key is written once: a key given twice keeps its first.
func UpsertMany(ctx context.Context, userID uuid.UUID, in []UpsertInput) ([]Upserted, error) {
	seen := map[string]bool{}
	var kinds, titles, bodies, ctaURLs, ctaTexts, srcTypes, srcIDs, keys []string
	var prios []int64
	for _, n := range in {
		if n.DedupKey == "" || seen[n.DedupKey] {
			continue
		}
		seen[n.DedupKey] = true
		kinds, titles, bodies = append(kinds, n.Kind), append(titles, n.Title), append(bodies, n.Body)
		ctaURLs, ctaTexts = append(ctaURLs, n.CTAURL), append(ctaTexts, n.CTAText)
		srcTypes, srcIDs = append(srcTypes, n.SourceType), append(srcIDs, n.SourceID)
		prios, keys = append(prios, int64(n.Priority)), append(keys, n.DedupKey)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, `
		INSERT INTO workspace_nudges (
			user_id, kind, title, body, cta_url, cta_text,
			source_type, source_id, status, priority, dedup_key
		)
		SELECT $1, k, t, b, NULLIF(cu, ''), NULLIF(ct, ''), st, si, 'open', p, d
		FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::int[], $10::text[])
		     AS x(k, t, b, cu, ct, st, si, p, d)
		ON CONFLICT (dedup_key) WHERE status = 'open'
		DO UPDATE SET
			title      = EXCLUDED.title,
			body       = EXCLUDED.body,
			cta_url    = EXCLUDED.cta_url,
			cta_text   = EXCLUDED.cta_text,
			priority   = EXCLUDED.priority,
			updated_at = NOW()
		RETURNING dedup_key, id, (xmax = 0)`, func(r pgexec.Row) (u Upserted, err error) {
		err = r.Scan(&u.DedupKey, &u.ID, &u.Inserted)
		return
	}, userID, pq.Array(kinds), pq.Array(titles), pq.Array(bodies), pq.Array(ctaURLs), pq.Array(ctaTexts),
		pq.Array(srcTypes), pq.Array(srcIDs), pq.Array(prios), pq.Array(keys))
}
