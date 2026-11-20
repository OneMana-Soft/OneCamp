package models

// Everyone's nudges checked, written and closed a pass at a time: one
// statement per step for every person (backend audit #36: each nudge was its
// own terminal check and its own upsert; NOTIF M11: each person was their
// own check, upsert and supersede).

import (
	"context"
	"time"

	nudgeDomain "github.com/akashc777/OneCamp/domain/WorkspaceNudge"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// keyPairs flattens each person's dedup keys into the parallel (person, key)
// arrays the queries unnest.
func keyPairs(keys map[uuid.UUID][]string) (users []uuid.UUID, dedupKeys []string) {
	for u, ks := range keys {
		for _, k := range ks {
			users, dedupKeys = append(users, u), append(dedupKeys, k)
		}
	}
	return users, dedupKeys
}

// LastHandled is when each person last dismissed or acted on each of their
// dedup keys, read in one statement for everyone; a key they never handled
// is absent.
func LastHandled(ctx context.Context, keys map[uuid.UUID][]string) (map[uuid.UUID]map[string]time.Time, error) {
	out := map[uuid.UUID]map[string]time.Time{}
	users, dedupKeys := keyPairs(keys)
	if len(dedupKeys) == 0 {
		return out, nil
	}
	type handled struct {
		user uuid.UUID
		key  string
		at   time.Time
	}
	rows, err := pgexec.Query(ctx, nudgeDomain.LastHandled,
		func(r pgexec.Row) (h handled, err error) { err = r.Scan(&h.user, &h.key, &h.at); return }, pq.Array(users), pq.Array(dedupKeys))
	if err != nil {
		return nil, err
	}
	for _, h := range rows {
		if out[h.user] == nil {
			out[h.user] = map[string]time.Time{}
		}
		out[h.user][h.key] = h.at
	}
	return out, nil
}

// Upserted is what UpsertMany did for one dedup key.
type Upserted struct {
	UserID   uuid.UUID
	DedupKey string
	ID       uuid.UUID
	Inserted bool
}

// UpsertMany is the engine's pass over everyone's nudges, in one statement
// (nudgeDomain.UpsertMany): each signal that still holds is a new open nudge
// for its person (UpsertInput.UserID), or refreshes the copy of the one
// already open, keeping its updated_at. Finding it open again is no news, so
// it isn't raised again: the Inbox badge counts an urgent nudge by when it
// was last raised, and moving updated_at on every pass brought it back after
// every pass (V-577). Nor does a pass make it more urgent than it is: one
// gone quiet (QuietUnraisedSince) stays quiet (V-581). A nudge closed since
// (resolved, dismissed or superseded) comes back as a new row, so it is
// raised now. Each dedup key is written once: a key given twice keeps its
// first.
func UpsertMany(ctx context.Context, in []UpsertInput) ([]Upserted, error) {
	seen := map[string]bool{}
	var users []uuid.UUID
	var kinds, titles, bodies, ctaURLs, ctaTexts, srcTypes, srcIDs, keys []string
	var prios []int64
	for _, n := range in {
		if n.DedupKey == "" || seen[n.DedupKey] {
			continue
		}
		seen[n.DedupKey] = true
		users = append(users, n.UserID)
		kinds, titles, bodies = append(kinds, n.Kind), append(titles, n.Title), append(bodies, n.Body)
		ctaURLs, ctaTexts = append(ctaURLs, n.CTAURL), append(ctaTexts, n.CTAText)
		srcTypes, srcIDs = append(srcTypes, n.SourceType), append(srcIDs, n.SourceID)
		prios, keys = append(prios, int64(n.Priority)), append(keys, n.DedupKey)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, nudgeDomain.UpsertMany, func(r pgexec.Row) (u Upserted, err error) {
		err = r.Scan(&u.UserID, &u.DedupKey, &u.ID, &u.Inserted)
		return
	}, pq.Array(users), pq.Array(kinds), pq.Array(titles), pq.Array(bodies), pq.Array(ctaURLs), pq.Array(ctaTexts),
		pq.Array(srcTypes), pq.Array(srcIDs), pq.Array(prios), pq.Array(keys))
}

// SupersedeStale closes every open nudge of anyone that isn't kept, in one
// statement (nudgeDomain.SupersedeStale): keep is each person's dedup keys
// that still hold, and a person absent from it keeps none. It answers the
// ones it closed, by whose they were, so each person's tabs are told which.
func SupersedeStale(ctx context.Context, keep map[uuid.UUID][]string) (map[uuid.UUID][]uuid.UUID, error) {
	users, dedupKeys := keyPairs(keep)
	return closedByUser(ctx, nudgeDomain.SupersedeStale, pq.Array(users), pq.Array(dedupKeys))
}
