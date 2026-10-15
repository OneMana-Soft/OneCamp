package Calendar

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	"github.com/google/uuid"
)

// withPage stands in a booking page and its owner's calendars, counting each
// read of the calendars (with Google linked, each is a Google call), on a
// cache of its own.
func withPage(t *testing.T, owner uuid.UUID, busy []Interval) *atomic.Int32 {
	t.Helper()
	oldCache, oldLoad, oldBusy := pageCache, loadLive, busyFor
	t.Cleanup(func() { pageCache, loadLive, busyFor = oldCache, oldLoad, oldBusy })
	pageCache = newBookingPagesCache()
	hours := WorkingHours{Days: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "17:00", TZ: "Asia/Kolkata"}
	loc, err := hours.Check()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(hours)
	loadLive = func(_ context.Context, slug string) (*livePage, error) {
		if slug != "intro-call" {
			return nil, ErrNoPage
		}
		return &livePage{page: &bookingModel.Page{Id: uuid.New(), UserId: owner, Slug: slug, Title: "Intro call",
			DurationMinutes: 30, Hours: raw, MaxDaysAhead: 30, Active: true}, hours: hours, loc: loc, name: "Sam Rivera"}, nil
	}
	reads := &atomic.Int32{}
	busyFor = func(_ context.Context, who string, _, _ time.Time) (Busy, error) {
		if who != owner.String() {
			t.Errorf("read the calendars of %s, not the page's owner", who)
		}
		reads.Add(1)
		return Busy{Intervals: busy}, nil
	}
	return reads
}

// CAL-BUG-14: the answer was kept by the hour of the window asked for, so a
// script asking for a different hour each time made a Google call each time.
// Two hundred looks from two hundred different moments now read the owner's
// calendars once.
func TestTwoHundredLooksReadTheCalendarsOnce(t *testing.T) {
	owner := uuid.New()
	now := time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC) // Tuesday, 11:30 in Kolkata
	reads := withPage(t, owner, []Interval{{Start: now.Add(26 * time.Hour), End: now.Add(27 * time.Hour)}})

	days := map[string]bool{}
	for i := 0; i < 200; i++ {
		from := now.Add(time.Duration(i) * 37 * time.Minute)
		p, err := GetPublicPage(context.Background(), "Intro-Call", from, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Slots) == 0 {
			t.Fatalf("look %d offered nothing", i)
		}
		first := p.Slots[0].Start.In(time.FixedZone("IST", 5*3600+1800))
		days[first.Format("2006-01-02")] = true
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("the calendars were read %d times, want 1", n)
	}
	if len(days) < 2 {
		t.Errorf("the looks covered %d day(s); the test should span several", len(days))
	}
}

// Visits that miss together share one read.
func TestLooksThatMissTogetherShareOneRead(t *testing.T) {
	owner := uuid.New()
	now := time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)
	reads := withPage(t, owner, nil)
	gate := make(chan struct{})
	slow := busyFor
	busyFor = func(ctx context.Context, who string, from, to time.Time) (Busy, error) {
		<-gate
		return slow(ctx, who, from, to)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := GetPublicPage(context.Background(), "intro-call", now.Add(time.Duration(i)*time.Hour), now); err != nil {
				t.Error(err)
			}
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	if n := reads.Load(); n != 1 {
		t.Fatalf("the calendars were read %d times, want 1", n)
	}
}

// A booking, a change to the page or a change to one of the owner's events
// shows at once: the next look reads the calendars again. A read that was
// under way when it happened keeps nothing.
func TestAChangeIsSeenAtOnce(t *testing.T) {
	owner := uuid.New()
	now := time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)
	reads := withPage(t, owner, nil)
	look := func() {
		t.Helper()
		if _, err := GetPublicPage(context.Background(), "intro-call", now, now); err != nil {
			t.Fatal(err)
		}
	}
	look()
	look()
	if n := reads.Load(); n != 1 {
		t.Fatalf("reads %d, want 1", n)
	}
	pageCache.forgetOwner(owner.String()) // booked, cancelled, or the page saved
	look()
	if n := reads.Load(); n != 2 {
		t.Fatalf("after a booking: reads %d, want 2", n)
	}
	pageCache.forgetBusy("someone-else", owner.String()) // one of the owner's events moved
	look()
	if n := reads.Load(); n != 3 {
		t.Fatalf("after an event moved: reads %d, want 3", n)
	}

	// A booking lands while a read is under way: that read is stale, and is
	// not kept.
	inner := busyFor
	busyFor = func(ctx context.Context, who string, from, to time.Time) (Busy, error) {
		pageCache.forgetOwner(owner.String())
		return inner(ctx, who, from, to)
	}
	pageCache.forgetOwner(owner.String())
	look()
	busyFor = inner
	look()
	if n := reads.Load(); n != 5 {
		t.Fatalf("a read under way at a booking was kept: reads %d, want 5", n)
	}
	// And a minute on, everything is read again.
	if _, err := GetPublicPage(context.Background(), "intro-call", now, now.Add(bookingCacheTTL)); err != nil {
		t.Fatal(err)
	}
	if n := reads.Load(); n != 6 {
		t.Fatalf("after a minute: reads %d, want 6", n)
	}
}

func TestAMissingPageIsNotKept(t *testing.T) {
	now := time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)
	withPage(t, uuid.New(), nil)
	for i := 0; i < 3; i++ {
		if _, err := GetPublicPage(context.Background(), "nobody-here", now, now); err != ErrNoPage {
			t.Fatalf("got %v, want ErrNoPage", err)
		}
	}
	if len(pageCache.pages) != 0 || len(pageCache.answers) != 0 {
		t.Errorf("kept %d pages and %d answers for a page that doesn't exist", len(pageCache.pages), len(pageCache.answers))
	}
}

// A page has one window a day it can book: whatever is asked for, the window
// starts at the start of a day in the owner's zone, between today and the
// horizon, and runs 31 days or to the horizon.
func TestPublicWindow(t *testing.T) {
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC) // 01:30 on the 14th in Kolkata
	until := now.Add(60 * 24 * time.Hour)                 // 01:30 on 13 December in Kolkata
	for _, c := range []struct {
		name     string
		from     time.Time
		day, end string
	}{
		{"the past is today", now.Add(-72 * time.Hour), "2026-10-14T00:00:00+05:30", "2026-11-14T00:00:00+05:30"},
		{"an hour into a day", now.Add(5 * time.Hour), "2026-10-14T00:00:00+05:30", "2026-11-14T00:00:00+05:30"},
		{"the next day", now.Add(30 * time.Hour), "2026-10-15T00:00:00+05:30", "2026-11-15T00:00:00+05:30"},
		{"near the horizon", until.Add(-48 * time.Hour), "2026-12-11T00:00:00+05:30", "2026-12-13T01:30:00+05:30"},
		{"past the horizon", now.AddDate(5, 0, 0), "2026-12-13T00:00:00+05:30", "2026-12-13T01:30:00+05:30"},
	} {
		day, end := publicWindow(c.from, now, until, kolkata)
		if got := day.In(kolkata).Format(time.RFC3339); got != c.day {
			t.Errorf("%s: starts %s, want %s", c.name, got, c.day)
		}
		if got := end.In(kolkata).Format(time.RFC3339); got != c.end {
			t.Errorf("%s: ends %s, want %s", c.name, got, c.end)
		}
	}
}

// A full cache keeps what it has: a flood of new windows never flushes the
// answers visitors are using, and room comes back as they expire.
func TestAFullCacheKeepsWhatItHas(t *testing.T) {
	c := newBookingPagesCache()
	now := time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)
	for i := 0; i < maxBookingAnswers; i++ {
		c.putAnswer("intro-call|"+strconv.Itoa(i), "owner", 0, &PublicPage{}, now)
	}
	c.putAnswer("flood|1", "owner", 0, &PublicPage{Title: "flood"}, now)
	if c.answer("flood|1", now) != nil || c.answer("intro-call|0", now) == nil || len(c.answers) != maxBookingAnswers {
		t.Fatalf("a full cache took a new answer or dropped a fresh one: %d kept", len(c.answers))
	}
	later := now.Add(bookingCacheTTL)
	c.putAnswer("flood|1", "owner", 0, &PublicPage{Title: "flood"}, later)
	if c.answer("flood|1", later) == nil || len(c.answers) != 1 {
		t.Errorf("expired answers weren't swept for a new one: %d kept", len(c.answers))
	}
}

// A read that couldn't reach the owner's Google calendar showed its time as
// free and was kept a minute, answers and all. Now no answer is kept from it,
// and the read itself only a few seconds: Google is asked again soon, and a
// link that keeps failing is still not asked once a look.
func TestAReadMissingGoogleIsNotKept(t *testing.T) {
	owner := uuid.New()
	now := time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)
	reads := withPage(t, owner, nil)
	inner := busyFor
	busyFor = func(ctx context.Context, who string, from, to time.Time) (Busy, error) {
		b, err := inner(ctx, who, from, to)
		b.GoogleUnread = true
		return b, err
	}
	for _, at := range []time.Duration{0, time.Second, 5 * time.Second} {
		if _, err := GetPublicPage(context.Background(), "intro-call", now, now.Add(at)); err != nil {
			t.Fatal(err)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("three looks in five seconds read the calendars %d times, want 1", n)
	}
	if _, err := GetPublicPage(context.Background(), "intro-call", now, now.Add(unreadBusyTTL+time.Second)); err != nil {
		t.Fatal(err)
	}
	if n := reads.Load(); n != 2 {
		t.Fatalf("a look after %s read the calendars %d times in all, want 2", unreadBusyTTL+time.Second, n)
	}
}

// A booking page's own bucket, for a flood from many addresses, counts only
// answers worked out afresh: looks served from the cache are free, so no one
// can hold a page at 429 for everyone by looking at it. Past the bucket, a
// fresh answer is refused with when to come back.
func TestOnlyFreshAnswersCountAgainstThePage(t *testing.T) {
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil // counted in this process, as when Redis is down
	owner := uuid.New()
	now := time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)
	withPage(t, owner, nil)
	look := func() error {
		_, err := GetPublicPage(context.Background(), "intro-call", now, now)
		return err
	}
	for i := 0; i < 3*bookingPageMissesPerMinute; i++ {
		if err := look(); err != nil {
			t.Fatalf("cached look %d: %v", i, err)
		}
	}
	for i := 1; i < bookingPageMissesPerMinute; i++ {
		pageCache.forgetBusy(owner.String()) // an event of the owner's changed
		if err := look(); err != nil {
			t.Fatalf("fresh answer %d: %v", i+1, err)
		}
	}
	pageCache.forgetBusy(owner.String())
	var busy *PageBusyError
	if err := look(); !errors.As(err, &busy) || busy.Limit.RetryAfterSeconds() <= 0 {
		t.Fatalf("fresh answer %d: %v, want the page busy with a wait", bookingPageMissesPerMinute+1, err)
	}
}

// The busy read asked up to the page's horizon from now, plus a day, so a
// look a second later needed a range a second longer, missed the busy times
// kept, and read the owner's calendars again: "once a minute" held only for
// looks at the same instant. The range runs whole days now.
func TestLooksSecondsApartShareOneRead(t *testing.T) {
	owner := uuid.New()
	now := time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)
	reads := withPage(t, owner, nil)
	for i := 0; i < 30; i++ {
		at := now.Add(time.Duration(i) * 1500 * time.Millisecond)
		if _, err := GetPublicPage(context.Background(), "intro-call", at.Add(time.Duration(i)*24*time.Hour), at); err != nil {
			t.Fatal(err)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("thirty looks over 45 seconds read the calendars %d times, want 1", n)
	}
}

// S1 (QA, 14 Oct): a booking checked its time with the owner's Google
// calendar unread as if it were free, and booked on top of Google events.
// Time that couldn't be checked is never booked; read whole, it is.
func TestABookingNeverTakesTimeTheCalendarCouldntCheck(t *testing.T) {
	owner := uuid.New()
	withPage(t, owner, nil)
	unread := true
	busyFor = func(context.Context, string, time.Time, time.Time) (Busy, error) {
		return Busy{GoogleUnread: unread}, nil
	}
	l, err := loadLive(context.Background(), "intro-call")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)
	from := time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC) // Wednesday, 09:30 in Kolkata
	slots, err := l.slots(context.Background(), from, from.Add(2*time.Hour), now)
	if !errors.Is(err, ErrCalendarUnread) || slots != nil {
		t.Fatalf("with Google unread: slots %v, err %v; want none and ErrCalendarUnread", slots, err)
	}
	unread = false
	slots, err = l.slots(context.Background(), from, from.Add(2*time.Hour), now)
	if err != nil || len(slots) == 0 || !slots[0].Start.Equal(from) {
		t.Fatalf("read whole: slots %v, err %v; want the slot at %s", slots, err, from)
	}
}
