package business

// Which versions to keep (docs programme A4): every version for a day, then the
// newest of each hour for a week, then the newest of each day up to the retention
// period, and nothing older. A version someone named or pinned is always kept. An
// agent run's version is kept whole for the week its Revert is offered. Within
// that, a resource's versions stay under a byte cap: past it, the oldest versions
// nobody kept go first, never the newest few.
//
// It was at most 15 versions a doc (about two and a half hours of a busy one) and
// an age sweep that deleted 200 a day; a named version was pruned like any other.
//
// planPrune is pure, so the rules are tested on their own; each kind applies the
// plan to its own store.

import (
	"context"
	"os"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	"github.com/google/uuid"
)

// Policy is how long versions are kept and how much room they may take.
type Policy struct {
	// Retention is how long a version nobody kept lasts.
	Retention time.Duration
	// MaxBytes caps a resource's versions in storage (stored, compressed).
	MaxBytes int64
	// KeepNewest versions are never pruned for room.
	KeepNewest int
}

const (
	everyVersionFor = 24 * time.Hour
	hourlyFor       = 7 * 24 * time.Hour
)

// historyDays is the workspace's setting, docs.history.days (a year unless an
// admin sets it). A seam for tests.
var historyDays = func(ctx context.Context) int {
	return policyBusiness.Policies.Int(ctx, policyBusiness.DocsHistoryDays)
}

// appliedDays is the retention the last scheduled prune applied; 0 before the
// first. A lowered setting removes nothing until that prune takes it, while a
// raised one keeps more at once.
var appliedDays atomic.Int64

// longestDays is what a doc's own prune keeps before any scheduled prune ran.
const longestDays = 3650

// docPolicy is the doc history's policy, for the prune after a version is
// kept: the setting, never below what the last scheduled prune applied (so a
// lower setting waits for it), and never below DOC_SNAPSHOT_RETENTION_DAYS, the
// server's floor. DOC_SNAPSHOT_MAX_MB caps each doc's versions (64 by default).
func docPolicy(ctx context.Context) Policy {
	applied := int(appliedDays.Load())
	if applied == 0 {
		applied = longestDays
	}
	return policyFor(max(historyDays(ctx), applied))
}

// scheduledDocPolicy is the policy the scheduled prune applies: the setting
// as it is now, which it records for the docs' own prunes after it.
func scheduledDocPolicy(ctx context.Context) Policy {
	p := policyFor(historyDays(ctx))
	appliedDays.Store(int64(p.Retention / (24 * time.Hour)))
	return p
}

func policyFor(days int) Policy {
	mb := int64(64)
	if n, err := strconv.Atoi(os.Getenv("DOC_SNAPSHOT_RETENTION_DAYS")); err == nil && n > days {
		days = n
	}
	if days < 1 {
		days = 1
	}
	if n, err := strconv.ParseInt(os.Getenv("DOC_SNAPSHOT_MAX_MB"), 10, 64); err == nil && n > 0 {
		mb = n
	}
	return Policy{Retention: time.Duration(days) * 24 * time.Hour, MaxBytes: mb << 20, KeepNewest: 3}
}

// planRow is what the plan decides by.
type planRow struct {
	ID        uuid.UUID
	AgentRun  bool
	Kept      bool
	Bytes     int64
	CreatedAt time.Time
}

// planPrune answers the versions to delete, of rows in any order.
func planPrune(rows []planRow, now time.Time, p Policy) []uuid.UUID {
	sorted := append([]planRow(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CreatedAt.After(sorted[j].CreatedAt) })

	keep := make([]bool, len(sorted))
	seenHour := map[time.Time]bool{}
	seenDay := map[time.Time]bool{}
	for i, r := range sorted {
		age := now.Sub(r.CreatedAt)
		switch {
		case r.Kept:
			keep[i] = true
		case age < everyVersionFor:
			keep[i] = true
		case age < hourlyFor:
			hour := r.CreatedAt.UTC().Truncate(time.Hour)
			keep[i] = r.AgentRun || !seenHour[hour]
			seenHour[hour] = true
		case age < p.Retention:
			day := r.CreatedAt.UTC().Truncate(24 * time.Hour)
			keep[i] = !seenDay[day]
			seenDay[day] = true
		}
	}

	// Room: the oldest versions nobody kept go first, never the newest few.
	var total int64
	for i, r := range sorted {
		if keep[i] {
			total += r.Bytes
		}
	}
	for i := len(sorted) - 1; i >= 0 && p.MaxBytes > 0 && total > p.MaxBytes; i-- {
		if keep[i] && !sorted[i].Kept && i >= p.KeepNewest {
			keep[i] = false
			total -= sorted[i].Bytes
		}
	}

	var drop []uuid.UUID
	for i, r := range sorted {
		if !keep[i] {
			drop = append(drop, r.ID)
		}
	}
	return drop
}
