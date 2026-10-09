// Package business (DemoGuard) keeps the public demo's own content from the
// demo's shared visitor.
//
// WHY. Everyone who opens the demo signs in as the same account, and the
// seeder (business/DemoSeed) makes the demo's channels, projects, team and
// docs AS that account, so the visitor owns them and the product lets an owner
// archive or delete. One visitor archiving #engineering, or the Q4 launch
// project, took it from every visitor after them until the nightly reset,
// along with the guide and the landing pages that send people to it.
//
// WHAT COUNTS AS THE DEMO'S. Anything that existed when the demo was last
// seeded: demoseed records the moment it finished (MarkSeeded, from
// services/DemoSeed), and anything created before then is the demo's. That is
// what the nightly reset restores and reseeds, and it needs no list of names
// to keep up to date, and survives a rename. What visitors make during the
// day is created after it, and stays theirs to archive or delete.
//
// FAILS CLOSED. With no record of a seeding (a server that was never seeded,
// or the minutes between the restore and the seeder finishing), or no
// creation time on the thing itself, it counts as the demo's: refusing a
// visitor's own archive until the next seeding is a nuisance, losing the
// demo's channel for a day is not.
//
// ONLY THE SHARED VISITOR, ONLY ON THE DEMO (helpers.IsDemoVisitor). The
// people who run the demo sign in as themselves and archive what they like,
// and on any other server this is never true.
package business

import (
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

// SeededAtKey is the system_configs key holding when the demo was last seeded,
// in RFC 3339.
const SeededAtKey = "demo_seeded_at"

// The seeding time is read at most this often: a refusal is rare, and the
// seeder runs as another process, so there is nothing to invalidate it from.
const cacheFor = 30 * time.Second

var (
	cacheMu    sync.Mutex
	cachedAt   time.Time
	cachedOK   bool
	cacheUntil time.Time
)

// readSeededAt reads the record. A seam, so the rule is tested without a database.
var readSeededAt = func() (time.Time, bool) {
	if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
		return time.Time{}, false
	}
	cfg, err := configModel.GetConfigByKey(SeededAtKey)
	if err != nil || cfg == nil {
		return time.Time{}, false
	}
	return parseSeededAt(cfg.Value)
}

// parseSeededAt is the record's value as a time, and whether it is one. Pure.
func parseSeededAt(v string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(v))
	if err != nil || t.IsZero() {
		return time.Time{}, false
	}
	return t, true
}

func seededAt(now time.Time) (time.Time, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if now.Before(cacheUntil) {
		return cachedAt, cachedOK
	}
	cachedAt, cachedOK = readSeededAt()
	cacheUntil = now.Add(cacheFor)
	return cachedAt, cachedOK
}

// forgetCache drops the cached seeding time. For tests.
func forgetCache() {
	cacheMu.Lock()
	cacheUntil = time.Time{}
	cacheMu.Unlock()
}

// isTheDemos is the rule: whether something created at created belongs to the
// demo, given when it was last seeded. Unknown either way is the demo's (see
// FAILS CLOSED). Pure.
func isTheDemos(created *time.Time, seeded time.Time, known bool) bool {
	if !known || created == nil || created.IsZero() {
		return true
	}
	return !created.After(seeded)
}

// KeepsFromVisitor reports whether the demo keeps something created at
// createdAt from the person with this email: they are the demo's shared
// visitor, and it is the demo's own. Always false for anyone else, and on any
// server that is not the demo.
func KeepsFromVisitor(email string, createdAt *time.Time) bool {
	if !helpers.IsDemoVisitor(email) {
		return false
	}
	at, known := seededAt(time.Now())
	return isTheDemos(createdAt, at, known)
}

// MarkSeeded records that the demo's content was put back at at. Everything
// created until then is the demo's; what visitors make afterwards is theirs.
func MarkSeeded(at time.Time) error {
	err := configModel.UpsertConfig(SeededAtKey, at.UTC().Format(time.RFC3339Nano))
	forgetCache()
	return err
}
