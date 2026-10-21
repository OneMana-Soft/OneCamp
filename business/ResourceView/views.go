package business

// A doc's views by day (docs C6-3), by members and by a published page's
// visitors, under the rules onemana.dev counts its own visits by:
//   - a member who chose "Don't record my views" is recorded nowhere: not in
//     "Viewed by", not by day;
//   - a visitor is counted under a keyed hash of their address and browser,
//     with a random salt that changes every UTC day and is never stored
//     anywhere but Redis, for two days: no address is kept, and nobody can be
//     followed from one day to the next;
//   - a request with Global Privacy Control or Do Not Track, or from a bot, is
//     not counted at all;
//   - a day's rows are swept after 400 days.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"regexp"
	"sync"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	viewModels "github.com/akashc777/OneCamp/models/postgres/ResourceView"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	redisRegistry "github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

// Retention: how long a day's rows are kept, and how many a pass removes.
const (
	keepDays    = 400
	pruneBudget = 5000
)

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "resource.view-days.retention", Every: 6 * time.Hour, Run: pruneOld})
}

// Seams for tests: the clock, a member's choice, the day's rows and the salt.
var (
	now          = time.Now
	recordsViews = memberRecordsViews
	recordView   = viewModels.RecordView
	recordDay    = viewModels.RecordDay
	daySalt      = sharedDaySalt
	pruneDays    = viewModels.PruneDays
)

// RELEASE-LINE ONLY (v2.75.0 / v1.60.0): a doc's views by day are written only
// where docs C6-3's web is: its insights read them, and a member turns them off
// there ("Don't record my views"). This release's web has neither, so the
// release lines write no day rows, for members or a published page's visitors,
// and "Viewed by" is recorded as in v2.74.2. Beta writes them; the release that
// carries C6-3's web sets this back (releaseLine_test.go holds it till then).
const recordsByDayOnTheReleaseLine = false

// recordsByDay is that switch, a seam for tests.
var recordsByDay = recordsByDayOnTheReleaseLine

// memberRecordsViews is a member's choice: on unless they turned it off. A
// member with no preferences yet has the defaults; a read that fails records
// nothing (their choice can't be known, so it isn't overridden).
func memberRecordsViews(ctx context.Context, userUUID string) bool {
	id, err := uuid.Parse(userUUID)
	if err != nil {
		return false
	}
	pref, err := prefModels.GetByUserID(id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return true
	case err != nil || pref == nil:
		return false
	}
	return pref.RecordViews
}

// RecordView records (deduplicated, throttled) that userUUID viewed a
// resource, in "Viewed by" and by day, unless they chose not to be recorded.
// The caller must have already verified the user can access the resource.
func RecordView(ctx context.Context, resourceType, resourceUUID, userUUID string) error {
	if !recordsViews(ctx, userUUID) {
		return nil
	}
	if err := recordView(ctx, resourceType, resourceUUID, userUUID); err != nil {
		return err
	}
	if !recordsByDay {
		return nil
	}
	return recordDay(ctx, resourceType, resourceUUID, viewModels.SourceMember, userUUID, now())
}

// botUA is a user agent that isn't a person reading: crawlers, link
// previews, monitors and headless browsers.
var botUA = regexp.MustCompile(`(?i)bot|crawl|spider|slurp|preview|facebookexternalhit|embedly|monitor|uptime|headless|lighthouse|curl/|wget/|python-requests|go-http-client|httpclient|java/|okhttp`)

// Visit is what counting a published page's visitor reads of the request.
type Visit struct {
	Address   string // helpers.ClientIP; only ever hashed
	UserAgent string
	GPC       bool // Sec-GPC: 1
	DNT       bool // DNT: 1
}

// Counted reports whether a visit may be counted at all. Pure.
func (v Visit) Counted() bool {
	if v.GPC || v.DNT || v.UserAgent == "" {
		return false
	}
	return !botUA.MatchString(v.UserAgent)
}

// CountPageVisit counts a published page's visitor for the day, when the
// visit may be counted. scope keeps one visitor's keys apart between pages.
func CountPageVisit(ctx context.Context, resourceType, resourceUUID, scope string, v Visit) error {
	if !recordsByDay || !v.Counted() || resourceUUID == "" {
		return nil
	}
	at := now().UTC()
	salt, err := daySalt(ctx, at.Format("2006-01-02"))
	if err != nil || len(salt) == 0 {
		return err
	}
	return recordDay(ctx, resourceType, resourceUUID, viewModels.SourcePage, visitorKey(salt, v, scope), at)
}

// visitorKey is a visitor's key for the day: an HMAC of their address and
// browser under the day's salt, never the address itself. Pure.
func visitorKey(salt []byte, v Visit, scope string) string {
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(v.Address + "\n" + v.UserAgent + "\n" + scope))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// localSalts are this process's salts, by day, for when Redis can't be asked:
// counting goes on, per instance, never with a salt anyone keeps.
var (
	localMu    sync.Mutex
	localSalts = map[string][]byte{}
)

// sharedDaySalt is the day's salt, the same for every instance: made once,
// kept in Redis for two days, never logged.
func sharedDaySalt(ctx context.Context, day string) ([]byte, error) {
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	// Whichever instance sets it first, every one reads the one kept.
	if _, err := redisStore.SetStringIfAbsent(ctx, redisRegistry.ViewSalt, []string{day}, hex.EncodeToString(fresh), redisRegistry.ViewSalt.TTL); err == nil {
		if got, ok, err := redisStore.GetString(ctx, redisRegistry.ViewSalt, []string{day}); err == nil && ok {
			if b, derr := hex.DecodeString(got); derr == nil {
				return b, nil
			}
		}
	}
	localMu.Lock()
	defer localMu.Unlock()
	for d := range localSalts {
		if d != day {
			delete(localSalts, d)
		}
	}
	if s, ok := localSalts[day]; ok {
		return s, nil
	}
	localSalts[day] = fresh
	return fresh, nil
}

// pruneOld removes a pass's worth of day rows past keepDays.
func pruneOld(ctx context.Context) error {
	n, err := pruneDays(ctx, now().UTC().AddDate(0, 0, -keepDays), pruneBudget)
	if err == nil && n > 0 {
		helpers.LogInfoWithContext(ctx, "resource views: %d day rows past %d days removed", n, keepDays)
	}
	return err
}
