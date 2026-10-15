package Calendar

// What a booking page shows is read once a minute, not once a visit
// (CAL-BUG-14).
//
// A booking page's free times come from its owner's calendars: OneCamp's
// events, and Google Calendar's, read with the owner's own token. The answer
// was cached by slug and by the hour of the window asked for, so a script
// asking for a different hour each time missed every time and spent the
// owner's Google quota as fast as it liked (their calendar then showed no
// Google events at all), and at 2,000 entries the cache emptied itself, for
// real visitors too. Now:
//
//   - a window starts at the start of a day in the owner's zone and runs 31
//     days, or to the page's horizon, so a page has one answer a day it can
//     book, not one an hour anyone asks for;
//   - the owner's busy times are read at most once a minute, over everything
//     the page can offer, and every window is cut from that one read; visits
//     that miss together share it;
//   - a booking, a cancellation, a change to the page, or a change to an event
//     of the owner's (booked or not) forgets them at once. An event made since
//     shows within the minute.
//
// Booking never trusts any of it: Book reads the page and the calendars again
// before it takes a slot.

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	bookingCacheTTL = time.Minute
	// maxBookingAnswers bounds the answers kept, and so the memory: an answer
	// is up to a month of slots, tens of kilobytes. Past it, expired answers
	// go first; while it's full of fresh ones a new answer isn't kept, and is
	// cut again next time from the busy times kept, which reads no calendar.
	maxBookingAnswers = 1000
	// busyReadTimeout bounds one shared read of someone's calendars, which
	// outlives the visit that started it.
	busyReadTimeout = 20 * time.Second
	// unreadBusyTTL keeps a read that couldn't reach a Google calendar: long
	// enough that a link that keeps failing is asked six times a minute, not
	// once a look; short enough that a passing error clears soon.
	unreadBusyTTL = 10 * time.Second
)

// Swapped in tests: how a page is loaded, and how busy times are read.
var (
	loadLive = loadPage
	busyFor  = BusyFor
)

type pageEntry struct {
	live    *livePage
	expires time.Time
}

type answerEntry struct {
	owner   string
	page    *PublicPage
	expires time.Time
}

type busyEntry struct {
	from, to time.Time
	busy     Busy
	expires  time.Time
}

// bookingPagesCache holds the three, by slug, by slug and day, and by owner.
// gen counts how often an owner's entries were forgotten, and epoch any
// page's: a read that began before a forget stores nothing, so it can't put
// back what the forget dropped.
type bookingPagesCache struct {
	mu      sync.Mutex
	pages   map[string]pageEntry
	answers map[string]answerEntry
	busy    map[string]busyEntry
	gen     map[string]uint64
	epoch   uint64
	flight  singleflight.Group
}

var pageCache = newBookingPagesCache()

func newBookingPagesCache() *bookingPagesCache {
	return &bookingPagesCache{pages: map[string]pageEntry{}, answers: map[string]answerEntry{},
		busy: map[string]busyEntry{}, gen: map[string]uint64{}}
}

// live is a page and its owner, loaded at most once a minute.
func (c *bookingPagesCache) live(ctx context.Context, slug string, now time.Time) (*livePage, error) {
	c.mu.Lock()
	e, ok := c.pages[slug]
	epoch := c.epoch
	c.mu.Unlock()
	if ok && now.Before(e.expires) {
		return e.live, nil
	}
	l, err := loadLive(ctx, slug)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.epoch == epoch {
		c.pages[slug] = pageEntry{live: l, expires: now.Add(bookingCacheTTL)}
	}
	c.mu.Unlock()
	return l, nil
}

// generation is how often owner's entries have been forgotten.
func (c *bookingPagesCache) generation(owner string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen[owner]
}

func (c *bookingPagesCache) answer(key string, now time.Time) *PublicPage {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.answers[key]; ok && now.Before(e.expires) {
		return e.page
	}
	return nil
}

// putAnswer keeps a page's answer for a minute, unless its owner's entries
// were forgotten since gen.
func (c *bookingPagesCache) putAnswer(key, owner string, gen uint64, p *PublicPage, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen[owner] != gen {
		return
	}
	if len(c.answers) >= maxBookingAnswers {
		for k, e := range c.answers {
			if !now.Before(e.expires) {
				delete(c.answers, k)
			}
		}
		if len(c.answers) >= maxBookingAnswers {
			return // never flush what visitors are using
		}
	}
	c.answers[key] = answerEntry{owner: owner, page: p, expires: now.Add(bookingCacheTTL)}
}

// busyOf is when owner is busy between from and to, read at most once a
// minute. A read covers every range asked for since the last one expired, so
// an owner's pages with different horizons share it; concurrent misses wait
// for one read rather than each making their own. A read with a Google
// calendar it couldn't read is kept only unreadBusyTTL, and no answer cut
// from it is kept at all.
func (c *bookingPagesCache) busyOf(ctx context.Context, owner string, from, to, now time.Time) (Busy, error) {
	if e, ok := c.cachedBusy(owner, now); ok && covers(e, from, to) {
		return e.busy, nil
	}
	v, err, _ := c.flight.Do(owner, func() (any, error) {
		lo, hi := from, to
		c.mu.Lock()
		gen := c.gen[owner]
		if e, ok := c.busy[owner]; ok && now.Before(e.expires) {
			if covers(e, from, to) {
				c.mu.Unlock()
				return e, nil // a read that landed while this one waited
			}
			if e.from.Before(lo) {
				lo = e.from
			}
			if e.to.After(hi) {
				hi = e.to
			}
		}
		c.mu.Unlock()
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), busyReadTimeout)
		defer cancel()
		busy, err := busyFor(rctx, owner, lo, hi)
		if err != nil {
			return nil, err
		}
		ttl := bookingCacheTTL
		if busy.GoogleUnread {
			ttl = unreadBusyTTL
		}
		e := busyEntry{from: lo, to: hi, busy: busy, expires: now.Add(ttl)}
		c.mu.Lock()
		if c.gen[owner] == gen {
			c.busy[owner] = e
		}
		c.mu.Unlock()
		return e, nil
	})
	if err != nil {
		return Busy{}, err
	}
	if e := v.(busyEntry); covers(e, from, to) {
		return e.busy, nil
	}
	// It joined a read of a narrower range (another of the owner's pages)
	// started a moment before: read this one itself.
	return busyFor(ctx, owner, from, to)
}

func (c *bookingPagesCache) cachedBusy(owner string, now time.Time) (busyEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.busy[owner]
	return e, ok && now.Before(e.expires)
}

func covers(e busyEntry, from, to time.Time) bool {
	return !e.from.After(from) && !e.to.Before(to)
}

// forgetOwner drops everything kept for an owner's pages: a page saved,
// deleted, booked or cancelled shows at once.
func (c *bookingPagesCache) forgetOwner(owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for slug, e := range c.pages {
		if e.live.page.UserId.String() == owner {
			delete(c.pages, slug)
		}
	}
	c.epoch++
	c.forgetBusyLocked(owner)
}

// forgetBusy drops what was read of these people's calendars, and the answers
// cut from it: an event of theirs changed.
func (c *bookingPagesCache) forgetBusy(people ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range people {
		if p != "" {
			c.forgetBusyLocked(p)
		}
	}
}

func (c *bookingPagesCache) forgetBusyLocked(owner string) {
	delete(c.busy, owner)
	for k, e := range c.answers {
		if e.owner == owner {
			delete(c.answers, k)
		}
	}
	c.gen[owner]++
}
