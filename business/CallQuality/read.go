package business

import (
	"context"
	"errors"
	"time"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	qualityModel "github.com/akashc777/OneCamp/models/postgres/CallQuality"
)

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "calls.quality.days", Every: time.Hour, RunAtStart: true, Run: func(ctx context.Context) error {
		_, rolled := RollUpDays(ctx, time.Now())
		_, purged := PurgeSamples(ctx, time.Now())
		return errors.Join(rolled, purged)
	}})
}

// The store's reads, seams for tests.
var reads = struct {
	tallies   func(context.Context, string, float32, int) ([]qualityModel.Tally, error)
	rollUp    func(context.Context, time.Time, float32, int) (bool, error)
	lastDay   func(context.Context) (time.Time, bool, error)
	firstSeen func(context.Context) (time.Time, bool, error)
	days      func(context.Context, time.Time, time.Time) ([]qualityModel.Day, error)
	purge     func(context.Context, time.Time, int) (int64, error)
}{
	tallies:   qualityModel.Tallies,
	rollUp:    qualityModel.RollUp,
	lastDay:   qualityModel.LastDay,
	firstSeen: qualityModel.FirstSample,
	days:      qualityModel.Days,
	purge:     qualityModel.Purge,
}

// PersonQuality is why a call stuttered for one person in it.
type PersonQuality struct {
	Identity string `json:"identity"`
	Name     string `json:"name"`
	Guest    bool   `json:"guest,omitempty"`
	Judgement
}

// Why says, for each person whose app sent samples of the call in session,
// why it stuttered for them. Whoever runs the call reads it (anyone in a DM or
// a group, a host or co-host elsewhere), and the workspace's admins.
func Why(ctx context.Context, actor *dgraphStruct.DgraphUser, admin bool, session string) ([]PersonQuality, error) {
	if helpers.ActsThroughToken(ctx) {
		return nil, ErrThroughToken
	}
	s, err := seams.session(ctx, session)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, ErrNotInCall
	}
	runs := false
	if actor != nil {
		if me := s.Person(actor.Uid); me != nil {
			runs = len(callControlBusiness.AbilitiesOf(s.Resource.Kind, me.Role)) > 0
		}
	}
	if !runs && !admin {
		return nil, ErrNotAllowed
	}
	tallies, err := reads.tallies(ctx, session, lossBad, rttBad)
	if err != nil {
		return nil, err
	}
	out := make([]PersonQuality, 0, len(tallies))
	for _, t := range tallies {
		p := PersonQuality{Identity: t.Identity, Judgement: judge(t)}
		if who := s.Person(p.Identity); who != nil {
			p.Name, p.Guest = who.Name, who.Guest
		}
		out = append(out, p)
	}
	return out, nil
}

// maxDaysAPass is how many days one pass rolls up, so a long gap is caught up
// over a few passes.
const maxDaysAPass = 60

// RollUpDays writes the install's row for each whole day (UTC) since the last
// one rolled up, or since the first sample, up to yesterday. A day that fails
// is passed over, so one bad day never holds the rest back. Answers how many
// rows it wrote.
func RollUpDays(ctx context.Context, now time.Time) (int, error) {
	yesterday := dayOf(now).AddDate(0, 0, -1)
	last, rolled, err := reads.lastDay(ctx)
	if err != nil {
		return 0, err
	}
	start := dayOf(last).AddDate(0, 0, 1)
	if !rolled {
		first, found, err := reads.firstSeen(ctx)
		if err != nil || !found {
			return 0, err
		}
		start = dayOf(first)
	}
	wrote, tried := 0, 0
	var firstErr error
	for d := start; !d.After(yesterday) && tried < maxDaysAPass; d = d.AddDate(0, 0, 1) {
		tried++
		ok, err := reads.rollUp(ctx, d, lossBad, rttBad)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallQuality: rolling up %s: %+v", d.Format(time.DateOnly), err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if ok {
			wrote++
		}
	}
	return wrote, firstErr
}

// keepSamples is how long a call's own samples stay for Why once their day
// is rolled up; the install's days keep how its calls went after that.
const keepSamples = 30 * 24 * time.Hour

// A pass deletes purgeBatch samples at a time, purgeBatches times at most,
// so a backlog goes over a few passes rather than in one long delete.
const (
	purgeBatch   = 10_000
	purgeBatches = 10
)

// PurgeSamples deletes the samples older than keepSamples whose day has been
// rolled up, a batch at a time. Answers how many it deleted.
func PurgeSamples(ctx context.Context, now time.Time) (int64, error) {
	last, rolled, err := reads.lastDay(ctx)
	if err != nil || !rolled {
		return 0, err
	}
	before := purgeBefore(now, last)
	var deleted int64
	for range purgeBatches {
		n, err := reads.purge(ctx, before, purgeBatch)
		deleted += n
		if err != nil || n < purgeBatch {
			return deleted, err
		}
	}
	return deleted, nil
}

// purgeBefore is the time before which samples go: keepSamples ago, but never
// past the end of the last day rolled up, whose samples are still to be
// counted. Pure.
func purgeBefore(now, lastRolled time.Time) time.Time {
	before := now.Add(-keepSamples)
	if end := dayOf(lastRolled).AddDate(0, 0, 1); end.Before(before) {
		return end
	}
	return before
}

// Days is the install's last n whole days, oldest first: what Admin › Calls shows.
func Days(ctx context.Context, n int, now time.Time) ([]qualityModel.Day, error) {
	n = min(max(n, 1), 365)
	to := dayOf(now)
	return reads.days(ctx, to.AddDate(0, 0, -n), to)
}

// dayOf is the UTC day t falls in. Pure.
func dayOf(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
