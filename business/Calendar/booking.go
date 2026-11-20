package Calendar

// Booking pages: Calendly's links, self-hosted. A person publishes
// /book/<slug> with a meeting length, working hours, a buffer around
// meetings, a minimum notice and how far ahead people may book. Outsiders see
// only free slots (from the availability engine), book one with a name and an
// email, and it lands on the owner's calendar as an ordinary OneCamp event
// (and on their Google Calendar when connected). The guest gets a link that
// cancels it; the owner hears about both.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userDgraphModels "github.com/akashc777/OneCamp/models/dgraph/User"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	authService "github.com/akashc777/OneCamp/services/Auth"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/google/uuid"
)

const (
	maxPagesPerPerson   = 10
	maxPublicRange      = 31 * 24 * time.Hour
	maxBookingsPerEmail = 3 // a day, per page
)

// PageInput is a booking page as its owner edits it.
type PageInput struct {
	Id               string       `json:"id,omitempty"`
	Slug             string       `json:"slug"`
	Title            string       `json:"title"`
	Description      string       `json:"description"`
	DurationMinutes  int          `json:"duration_minutes"`
	Hours            WorkingHours `json:"hours"`
	BufferMinutes    int          `json:"buffer_minutes"`
	MinNoticeMinutes int          `json:"min_notice_minutes"`
	MaxDaysAhead     int          `json:"max_days_ahead"`
	Active           bool         `json:"active"`
	// Questions are what the page asks whoever books it, in its order.
	Questions []QuestionInput `json:"questions"`
	// TeamMode is "solo" (the default) or "collective": the owner and every
	// host who accepted, booked together. Left blank on an update, it is
	// the page's own (the web's editor sends none).
	TeamMode string `json:"team_mode"`
	// Hosts are the members the owner asks to host the page with them. Left
	// out (nil, not []) on an update, the page's hosts stay as they are.
	Hosts []string `json:"hosts"`
	// Owner is the saving owner as their session holds them, never the
	// request's: the name a newly invited host's notice gives. Left nil (a
	// seed, a test), that notice reads the name once.
	Owner *dgraphStruct.DgraphUser `json:"-"`
}

var slugJunk = regexp.MustCompile(`[^a-z0-9]+`)

// NormaliseSlug turns what someone typed into a page address: lower case,
// letters and digits joined by single dashes. Pure.
func NormaliseSlug(s string) string {
	return strings.Trim(slugJunk.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-"), "-")
}

// CheckPage validates a page and returns it ready to store; now is when,
// so a date of the page's own already gone by is refused. Pure.
func CheckPage(in PageInput, now time.Time) (bookingModel.Page, error) {
	var p bookingModel.Page
	p.Slug = NormaliseSlug(in.Slug)
	if n := len(p.Slug); n < 3 || n > 40 {
		return p, &AvailabilityError{Msg: "Use 3 to 40 letters, digits or dashes for the address."}
	}
	p.Title = strings.Join(strings.Fields(in.Title), " ")
	if p.Title == "" || utf8.RuneCountInString(p.Title) > 80 {
		return p, &AvailabilityError{Msg: "Give the page a title of up to 80 characters."}
	}
	p.Description = strings.TrimSpace(in.Description)
	if utf8.RuneCountInString(p.Description) > 1000 {
		return p, &AvailabilityError{Msg: "Keep the description under 1,000 characters."}
	}
	if in.DurationMinutes < 10 || in.DurationMinutes > 480 {
		return p, &AvailabilityError{Msg: "Meetings run from 10 minutes to 8 hours."}
	}
	if in.BufferMinutes < 0 || in.BufferMinutes > 240 {
		return p, &AvailabilityError{Msg: "Keep the buffer between 0 and 4 hours."}
	}
	if in.MinNoticeMinutes < 0 || in.MinNoticeMinutes > 30*24*60 {
		return p, &AvailabilityError{Msg: "Notice can be up to 30 days."}
	}
	if in.MaxDaysAhead < 1 || in.MaxDaysAhead > 180 {
		return p, &AvailabilityError{Msg: "Let people book 1 to 180 days ahead."}
	}
	loc, err := in.Hours.Check()
	if err != nil {
		return p, err
	}
	if day, past := in.Hours.PastDate(now.In(loc)); past {
		return p, &AvailabilityError{Msg: day + " has gone by: take it off the page's dates."}
	}
	if p.Questions, p.KeptQuestionIds, err = checkQuestions(in.Questions, in.Id == ""); err != nil {
		return p, err
	}
	if p.TeamMode, p.HostIds, err = checkTeam(in); err != nil {
		return p, err
	}
	hours, _ := json.Marshal(in.Hours)
	p.Hours = hours
	p.DurationMinutes, p.BufferMinutes = in.DurationMinutes, in.BufferMinutes
	p.MinNoticeMinutes, p.MaxDaysAhead, p.Active = in.MinNoticeMinutes, in.MaxDaysAhead, in.Active
	if in.Id != "" {
		id, err := uuid.Parse(in.Id)
		if err != nil {
			return p, &AvailabilityError{Msg: "That isn't one of your pages."}
		}
		p.Id = id
	}
	return p, nil
}

// SaveBookingPage creates or updates one of the person's pages. An update
// that leaves out the team mode or the hosts keeps the page's own.
func SaveBookingPage(ctx context.Context, owner uuid.UUID, in PageInput) (*bookingModel.Page, error) {
	stored, err := pageKeptFrom(owner, in)
	if err != nil {
		return nil, err
	}
	if stored != nil && strings.TrimSpace(in.TeamMode) == "" {
		in.TeamMode = stored.TeamMode
	}
	p, err := CheckPage(in, time.Now())
	if err != nil {
		return nil, err
	}
	p.UserId = owner
	if err := checkHosts(ctx, owner, p.HostIds); err != nil {
		return nil, err
	}
	if p.Id == uuid.Nil {
		if n, err := bookingModel.CountPages(owner); err != nil {
			return nil, err
		} else if n >= maxPagesPerPerson {
			return nil, &AvailabilityError{Msg: fmt.Sprintf("You have %d booking pages. Delete one to make another.", n)}
		}
	}
	saved, err := bookingModel.SavePage(p)
	if errors.Is(err, bookingModel.ErrSlugTaken) {
		// A clash, the person's to sort out: a 409 naming the address
		// (helpers.WriteWriteFailure), not a 503 or a bare 400.
		return nil, helpers.Conflict(fmt.Sprintf("Another booking page already uses the address %q. Pick another.", p.Slug), err)
	}
	if err == nil && saved == nil {
		// The update keeps a question only when it is the page's now: a
		// kept one that isn't was edited away since the page was opened.
		if len(p.KeptQuestionIds) > 0 {
			return nil, &AvailabilityError{Msg: "A question on this page changed since you opened it. Reload the page and try again."}
		}
		return nil, &AvailabilityError{Msg: "That isn't one of your pages."}
	}
	if err != nil {
		return nil, err
	}
	if keepsHosts(stored, in.Hosts, p.TeamMode) {
		pageCache.forgetOwner(owner.String())
		return saved, nil
	}
	// The host list as the owner left it: who is new is invited and told,
	// who is off it leaves (a solo page has nobody).
	invited, err := bookingModel.SetHosts(ctx, saved.Id, p.HostIds)
	pageCache.forgetOwner(owner.String())
	if err != nil {
		return nil, err
	}
	tellInvited(ctx, saved, in.Owner, invited)
	return saved, nil
}

// DeleteBookingPage deletes one of the person's pages, and says how many of
// its bookings are still to come; false when it isn't theirs. The page stops
// taking bookings and leaves their list, while its bookings stay on their
// calendar with their guests' links working (CAL-BUG-3): deleting a page
// used to delete every booking made through it.
func DeleteBookingPage(ctx context.Context, owner, id uuid.UUID) (bool, int, error) {
	ok, upcoming, err := bookingModel.DeletePage(owner, id)
	if ok {
		pageCache.forgetOwner(owner.String())
	}
	return ok, upcoming, err
}

// PublicPage is what a visitor sees: never the owner's events, only free slots.
type PublicPage struct {
	Slug            string     `json:"slug"`
	Title           string     `json:"title"`
	Description     string     `json:"description"`
	DurationMinutes int        `json:"duration_minutes"`
	OwnerName       string     `json:"owner_name"`
	OwnerTZ         string     `json:"owner_tz"`
	Slots           []Interval `json:"slots"`
	// BookableUntil is the last moment a slot may end, so the visitor's
	// date picker stops there.
	BookableUntil time.Time `json:"bookable_until"`
	// Questions are what the page asks whoever books it; nobody's answers.
	Questions bookingModel.Questions `json:"questions"`
	// HostNames are a team page's accepted hosts, by name alone.
	HostNames []string `json:"host_names,omitempty"`
}

// ErrNoPage is a page that doesn't exist or is switched off; both answer
// the same, so a closed page reveals nothing.
var ErrNoPage = errors.New("no such booking page")

type livePage struct {
	page  *bookingModel.Page
	hours WorkingHours
	loc   *time.Location // the hours' zone
	owner *model.UserInfo
	name  string
	// hosts are a team page's host rows, every state, with their names.
	hosts []bookingModel.Host
}

func loadPage(ctx context.Context, slug string) (*livePage, error) {
	p, err := bookingModel.PageBySlug(NormaliseSlug(slug))
	if err != nil {
		return nil, err
	}
	return livePageOf(ctx, p)
}

// livePageOf is page p as it takes bookings, or ErrNoPage: closed, its hours
// unreadable, or its owner gone from the workspace.
func livePageOf(ctx context.Context, p *bookingModel.Page) (*livePage, error) {
	if p == nil || !pageLive(p) {
		return nil, ErrNoPage
	}
	var hours WorkingHours
	if json.Unmarshal(p.Hours, &hours) != nil {
		return nil, ErrNoPage
	}
	loc, err := hours.Check()
	if err != nil {
		return nil, ErrNoPage
	}
	du, err := userDomain.GetDgraphUserInfoByUUID(ctx, p.UserId.String())
	// A read that failed is tried again, never "no such page".
	if err != nil && !errors.Is(err, userDgraphModels.ErrNotFound) {
		return nil, err
	}
	// Someone who has left the workspace takes no more bookings.
	if du == nil || helpers.IsSoftDeleted(du.DeletedAt) {
		return nil, ErrNoPage
	}
	var hosts []bookingModel.Host
	if p.TeamMode == teamCollective {
		if hosts, err = hostsOf(ctx, []uuid.UUID{p.Id}); err != nil {
			return nil, err
		}
	}
	return &livePage{page: p, hours: hours, loc: loc, name: bookingOwnerName(du), hosts: hosts,
		owner: &model.UserInfo{UserPostgresInfo: model.User{Id: p.UserId}, UserDgraphInfo: *du}}, nil
}

func (l *livePage) rules(now time.Time) SlotRules {
	return SlotRules{
		Duration: time.Duration(l.page.DurationMinutes) * time.Minute,
		Buffer:   time.Duration(l.page.BufferMinutes) * time.Minute,
		Earliest: now.Add(time.Duration(l.page.MinNoticeMinutes) * time.Minute),
	}
}

func (l *livePage) horizon(now time.Time) time.Time {
	return now.Add(time.Duration(l.page.MaxDaysAhead) * 24 * time.Hour)
}

// ErrCalendarUnread is a booking refused because the owner's calendars
// couldn't all be read: a linked Google calendar didn't answer, so the time
// may already be taken there. The visitor is asked to try again.
var ErrCalendarUnread = errors.New("the calendar couldn't be checked")

// slotTimes is the slot a booking of start takes: start to the minute, for
// the page's length, or why not: past, inside the page's notice or past its
// horizon. A booking and a move both ask it before the owner's calendars are
// read, so a time the page never takes costs no read (V-1194).
func (l *livePage) slotTimes(start, now time.Time) (time.Time, time.Time, error) {
	start = start.UTC().Truncate(time.Minute)
	end := start.Add(time.Duration(l.page.DurationMinutes) * time.Minute)
	if start.Before(now) {
		return start, end, &AvailabilityError{Msg: "That time has passed. Pick another."}
	}
	if start.Before(l.rules(now).Earliest) {
		return start, end, &AvailabilityError{Msg: "That time is too soon. Pick a later one."}
	}
	if end.After(l.horizon(now)) {
		return start, end, &AvailabilityError{Msg: "That time is too far ahead. Pick another."}
	}
	return start, end, nil
}

// slotFree is whether start..end is one of the page's free slots, the
// calendars read now: what a booking or a move checks, never the cache. own
// is the event a move moves, no clash with itself: it alone is left out of
// what's busy (nil for a new booking). Time a calendar couldn't be read for
// is never free: a booking would land on top of whatever is there.
func (l *livePage) slotFree(ctx context.Context, start, end, now time.Time, own *dgraphStruct.DgraphEvent) error {
	if slotChecking != nil {
		slotChecking()
	}
	from, to := start.Add(-time.Hour), end.Add(time.Hour)
	// The owner's calendars and every accepted host's, each leaving the
	// moved event out.
	busy, err := teamBusy(ctx, l.people(), func(ctx context.Context, who string) (Busy, error) {
		return readBusy(ctx, who, from.Add(-24*time.Hour), to.Add(24*time.Hour), BusyOptions{Except: own})
	})
	if err != nil {
		return err
	}
	if busy.GoogleUnread {
		return ErrCalendarUnread
	}
	for _, s := range FreeSlots(busy.Intervals, from, to, l.hours, l.rules(now)) {
		if s.Start.Equal(start) {
			return nil
		}
	}
	return &AvailabilityError{Msg: "That time was just taken. Pick another."}
}

// publicWindow is the window a look at a page from `from` is answered with:
// from the start of from's day in the owner's zone (from kept between now
// and the page's horizon), for 31 days or to the horizon. So a page has one
// window a day it can book, whatever anyone asks for. Pure.
func publicWindow(from, now, until time.Time, loc *time.Location) (time.Time, time.Time) {
	if from.Before(now) {
		from = now
	}
	if from.After(until) {
		from = until
	}
	day := helpers.DayStart(from, loc)
	to := day.AddDate(0, 0, int(maxPublicRange/(24*time.Hour)))
	if to.After(until) {
		to = until
	}
	return day, to
}

// bookingPageMissesPerMinute bounds how often one booking page's answer is
// worked out afresh, for everyone together: ten times what one address may
// ask (middleware.BookingPageReadLimit), so a flood from many addresses is
// held back while a busy page's visitors, served from the cache, never meet
// it.
const bookingPageMissesPerMinute = 600

// PageBusyError is a page worked out afresh too often this minute: 429, with
// Limit's wait.
type PageBusyError struct{ Limit redisStore.RateLimitResult }

func (e *PageBusyError) Error() string { return "booking page busy" }

// pageMissAllowed counts one fresh answer of page id, in a one-minute window,
// in this process when Redis can't be asked. Only real pages are counted.
func pageMissAllowed(ctx context.Context, id string) redisStore.RateLimitResult {
	rctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	return redisStore.AllowFixedWindowOrLocal(rctx, registry.PublicReadRate, []string{"booking-page-miss", id}, bookingPageMissesPerMinute)
}

// bookingMovesPerDay is how often a guest's link may move its booking in
// registry.BookingMoveRate's day: each move tells the page's owner (an inbox
// notice, an email, a push) and mails the guest, so one link can't flood an
// owner or spend the workspace's email.
const bookingMovesPerDay = 3

// MovedTooOftenError is a booking its link has moved bookingMovesPerDay times
// this day: 429, with Limit's wait.
type MovedTooOftenError struct{ Limit redisStore.RateLimitResult }

func (e *MovedTooOftenError) Error() string { return "booking moved too often" }

// moveAllowed charges one move of booking id (its id, never its link's
// secret) to its day, counted in this process when Redis can't be asked.
func moveAllowed(ctx context.Context, id string) redisStore.RateLimitResult {
	rctx, cancel := context.WithTimeout(ctx, redisStore.LimitAskTimeout)
	defer cancel()
	return redisStore.AllowFixedWindowOrLocal(rctx, registry.BookingMoveRate, []string{id}, bookingMovesPerDay)
}

// GetPublicPage is a page and its free slots from the start of from's day in
// the owner's zone, for 31 days or to the page's horizon. Answers are kept a
// minute (bookingCache.go), so looking again, or at another week, reads the
// owner's calendars at most once a minute.
func GetPublicPage(ctx context.Context, slug string, from, now time.Time) (*PublicPage, error) {
	slug = NormaliseSlug(slug)
	l, err := pageCache.live(ctx, slug, now)
	if err != nil {
		return nil, err
	}
	until := l.horizon(now)
	day, to := publicWindow(from, now, until, l.loc)
	key := slug + "|" + day.Format("2006-01-02")
	if p := pageCache.answer(key, now); p != nil {
		return p, nil
	}
	if res := pageMissAllowed(ctx, l.page.Id.String()); !res.Allowed {
		return nil, &PageBusyError{Limit: res}
	}
	people := l.people()
	gen := pageCache.generation(people...)
	out := &PublicPage{
		Slug: l.page.Slug, Title: l.page.Title, Description: l.page.Description,
		DurationMinutes: l.page.DurationMinutes, OwnerName: l.name, OwnerTZ: l.hours.TZ,
		Slots: []Interval{}, BookableUntil: until, Questions: l.page.Questions, HostNames: l.hostNames(),
	}
	if to.After(day) {
		// Everything the page can offer, so every window comes from one read:
		// whole days, from the day before today to the day after the
		// horizon's, so a look a moment later asks the same range.
		lo := helpers.DayStart(now, l.loc).Add(-24 * time.Hour)
		hi := helpers.DayStart(until, l.loc).AddDate(0, 0, 2)
		busy, err := teamBusy(ctx, people, func(ctx context.Context, who string) (Busy, error) {
			return pageCache.busyOf(ctx, who, lo, hi, now)
		})
		if err != nil {
			return nil, err
		}
		if slots := FreeSlots(busy.Intervals, day, to, l.hours, l.rules(now)); slots != nil {
			out.Slots = slots
		}
		if busy.GoogleUnread {
			return out, nil // read in part: no answer is kept from it
		}
	}
	pageCache.putAnswer(key, people[0], gen, out, now, people[1:]...)
	return out, nil
}

// BookInput is what a visitor sends to book a slot.
type BookInput struct {
	Start time.Time `json:"start"`
	Name  string    `json:"name"`
	Email string    `json:"email"`
	Note  string    `json:"note"`
	// TZ is the visitor's zone, to write their confirmation in their time.
	TZ string `json:"tz"`
	// Answers are the visitor's to the page's questions.
	Answers []GuestAnswer `json:"answers"`
}

// Booked is what the visitor gets back.
type Booked struct {
	// BookingID names the booking's calendar invite (UID <id>@onecamp): the
	// emailed one and the page's own .ics are one event.
	BookingID   string    `json:"booking_id"`
	Title       string    `json:"title"`
	OwnerName   string    `json:"owner_name"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	CancelToken string    `json:"cancel_token"`
	// Emailed is whether a confirmation went to the visitor; when not, the
	// page offers the calendar file and the cancel link itself.
	Emailed bool `json:"emailed"`
	// OwnerTold is whether the owner was told: an inbox item, or an email
	// queued. The page says they were only when they were.
	OwnerTold bool `json:"owner_told"`
}

// CheckGuest tidies and validates a visitor's details. Pure.
func CheckGuest(in BookInput) (BookInput, error) {
	if len(in.Answers) > maxQuestions {
		return in, &AvailabilityError{Msg: fmt.Sprintf("A page asks up to %d questions. Reload the page and try again.", maxQuestions)}
	}
	in.Name = oneLine(in.Name)
	if in.Name == "" || utf8.RuneCountInString(in.Name) > 100 {
		return in, &AvailabilityError{Msg: "Enter your name."}
	}
	if err := refuseHidden(in.Name, "your name"); err != nil {
		return in, err
	}
	addr, err := mail.ParseAddress(strings.TrimSpace(in.Email))
	if err != nil || len(addr.Address) > 254 || !strings.Contains(addr.Address[strings.LastIndex(addr.Address, "@")+1:], ".") {
		return in, &AvailabilityError{Msg: "Enter an email address like name@example.com."}
	}
	in.Email = addr.Address
	in.Note = strings.TrimSpace(lineBreaks.Replace(in.Note))
	if utf8.RuneCountInString(in.Note) > 1000 {
		return in, &AvailabilityError{Msg: "Keep the note under 1,000 characters."}
	}
	if err := refuseHidden(in.Note, "your note"); err != nil {
		return in, err
	}
	// The zone the guest reads the booking's times in, kept with the booking:
	// one Go knows, and never "Local", the server's own and nobody's.
	if loc, ok := helpers.FirstLocation(in.TZ); ok {
		in.TZ = loc.String()
	} else {
		in.TZ = "UTC"
	}
	return in, nil
}

// FormatWhen writes a meeting's time the way people read it, in a zone:
// "Tuesday 6 October 2026, 10:00–10:30 (Asia/Kolkata)". Pure.
func FormatWhen(start, end time.Time, tz string) string {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc, tz = time.UTC, "UTC"
	}
	s, e := start.In(loc), end.In(loc)
	return fmt.Sprintf("%s, %s–%s (%s)", s.Format("Monday 2 January 2006"), s.Format("15:04"), e.Format("15:04"), tz)
}

// Book takes a slot for a visitor. The slot must still be free when asked:
// the page's own bookings are checked under a lock, the calendar just before.
func Book(ctx context.Context, slug string, in BookInput, now time.Time) (*Booked, error) {
	in, err := CheckGuest(in)
	if err != nil {
		return nil, err
	}
	l, err := loadPage(ctx, slug)
	if err != nil {
		return nil, err
	}
	// Against the page's questions as they are now; kept with their labels.
	answers, err := CheckAnswers(l.page.Questions, in.Answers)
	if err != nil {
		return nil, err
	}
	if n, err := bookingModel.CountRecentByEmail(l.page.Id, in.Email, now.Add(-24*time.Hour)); err != nil {
		return nil, err
	} else if n >= maxBookingsPerEmail {
		return nil, &AvailabilityError{Msg: "You've booked this page a few times today. Try again tomorrow."}
	}

	start, end, err := l.slotTimes(in.Start, now)
	if err != nil {
		return nil, err
	}
	if err := l.slotFree(ctx, start, end, now, nil); err != nil {
		return nil, err
	}

	token, hash, sealed, err := helpers.NewSealedLinkSecret()
	if err != nil {
		return nil, err
	}
	b, err := bookingModel.Reserve(bookingModel.Booking{
		PageId: l.page.Id, GuestName: in.Name, GuestEmail: in.Email, Note: in.Note, StartsAt: start, EndsAt: end,
		GuestTZ: in.TZ, CancelHash: hash, CancelSealed: sealed, Answers: answers,
	})
	if errors.Is(err, bookingModel.ErrSlotTaken) {
		return nil, &AvailabilityError{Msg: "That time was just taken. Pick another."}
	}
	if err != nil {
		return nil, err
	}

	desc := bookingDescription(l.page.Title, in.Name, in.Email, in.Note, answers)
	lines := AnswerLines(answers)
	ev, err := CreateEvent(ctx, l.owner, adapter.CreateOrUpdateEventInput{
		Title:                fmt.Sprintf("%s with %s", l.page.Title, in.Name),
		Description:          desc,
		StartTime:            start.Format(time.RFC3339),
		EndTime:              end.Format(time.RFC3339),
		SyncToGoogleCalendar: true,
		// A team page's accepted hosts are on the meeting: busy for each.
		Participants: l.hostIDs(),
	})
	if err != nil {
		// Left behind, the booking would hold the slot with no meeting on it.
		if ferr := bookingModel.Forget(b.Id); ferr != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar/Book forget booking %s err: %+v", b.Id, ferr)
		}
		return nil, err
	}
	if id, err := uuid.Parse(ev.EventUuid); err == nil {
		if err := bookingModel.SetEvent(b.Id, id); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar/Book link event err: %+v", err)
		}
	}
	pageCache.forgetOwner(l.page.UserId.String())
	pageCache.forgetBusy(l.hostIDs()...)
	planGuestReminders(ctx, b, l.page, false, now)

	told := notificationBusiness.TellBookingOwner(ctx, notificationBusiness.BookingNotice{
		OwnerUUID: l.page.UserId.String(), EventUUID: ev.EventUuid, BookingID: b.Id.String(),
		GuestName: in.Name, PageTitle: l.page.Title, Start: start, End: end,
		TZ: l.hours.TZ, When: FormatWhen(start, end, l.hours.TZ), Answers: lines, HostUUIDs: l.hostIDs(),
	})
	emailed := mailGuest(ctx, guestMail{
		to: in.Email, name: in.Name,
		subject:  fmt.Sprintf("Booked: %s with %s", l.page.Title, l.name),
		title:    fmt.Sprintf("You're booked with %s", l.name),
		subtitle: l.page.Title,
		body:     FormatWhen(start, end, in.TZ),
		ctaText:  "Cancel this booking", ctaURL: cancelURL(token),
		footer: "You booked this time through a OneCamp booking page. Its calendar invite is attached.",
		invite: bookingInvite(inviteRequest, b, l.page.Title, l.name, true, now),
	})
	return &Booked{Title: l.page.Title, OwnerName: l.name, Start: start, End: end, CancelToken: token,
		BookingID: b.Id.String(), Emailed: emailed, OwnerTold: told}, nil
}

// BookingView is what a cancel link shows.
type BookingView struct {
	// BookingID names the booking's calendar invite (UID <id>@onecamp), the
	// one the page's own .ics uses too.
	BookingID string    `json:"booking_id"`
	Title     string    `json:"title"`
	OwnerName string    `json:"owner_name"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Cancelled bool      `json:"cancelled"`
	Slug      string    `json:"slug"`
	// PageLive is whether the page still takes bookings: the page offers
	// "Book another time" only then.
	PageLive bool `json:"page_live"`
	// CancelledByHost is a booking its host cancelled, by deleting the
	// meeting: the page says so rather than the guest's own cancel's words.
	CancelledByHost bool `json:"cancelled_by_host"`
	// OwnerTold, on a cancel's or a move's answer, is whether the owner was told of it:
	// an inbox item, or an email queued.
	OwnerTold bool `json:"owner_told,omitempty"`
	// Answers are what the guest answered the page's questions: for the
	// holder of the booking's link alone.
	Answers bookingModel.Answers `json:"answers"`
}

// tokenBooking is a booking found by its guest's link: the booking, its page,
// the owner's name, and whether the page still takes bookings.
type tokenBooking struct {
	b     *bookingModel.Booking
	p     *bookingModel.Page
	owner string
	live  bool
}

func (t *tokenBooking) view() *BookingView {
	return &BookingView{BookingID: t.b.Id.String(), Title: t.p.Title, OwnerName: t.owner, Start: t.b.StartsAt,
		End: t.b.EndsAt, Cancelled: t.b.CancelledAt != nil, Slug: t.p.Slug, PageLive: t.live,
		CancelledByHost: t.b.CancelledAt != nil && t.b.CancelledBy != nil && *t.b.CancelledBy == bookingModel.CancelledByHost,
		Answers:         t.b.Answers}
}

func bookingForToken(ctx context.Context, token string) (*tokenBooking, error) {
	b, err := bookingModel.BookingByLink(helpers.LinkSecretHash(token))
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, ErrNoPage
	}
	p, err := bookingModel.PageById(b.PageId)
	if err != nil || p == nil {
		return nil, ErrNoPage
	}
	// A deleted page keeps its bookings and their links, but takes no more.
	t := &tokenBooking{b: b, p: p, live: pageLive(p)}
	du, err := userDomain.GetDgraphUserInfoByUUID(ctx, p.UserId.String())
	if err == nil && du != nil {
		t.owner = bookingOwnerName(du)
	}
	// Someone who has left the workspace takes no more bookings (loadPage).
	if err != nil || du == nil || helpers.IsSoftDeleted(du.DeletedAt) {
		t.live = false
	}
	return t, nil
}

// bookingOwnerName is the name a booking page shows for its owner: the one
// name rule (helpers.PersonDisplayName) without its last step, since whoever
// books is outside the workspace and is not shown part of the owner's address.
func bookingOwnerName(u *dgraphStruct.DgraphUser) string {
	return helpers.PersonDisplayName(u.UserName, u.UserFullName, "")
}

// GetBooking is the booking behind a cancel link.
func GetBooking(ctx context.Context, token string) (*BookingView, error) {
	t, err := bookingForToken(ctx, token)
	if err != nil {
		return nil, err
	}
	return t.view(), nil
}

// CancelBooking cancels through the guest's link: the event comes off the
// owner's calendar, they hear about it, and so does the guest's calendar.
// Cancelling twice is not an error.
func CancelBooking(ctx context.Context, token string) (*BookingView, error) {
	t, err := bookingForToken(ctx, token)
	if err != nil {
		return nil, err
	}
	b, p, name := t.b, t.p, t.owner
	view := t.view()
	view.Cancelled = true
	changed, err := bookingModel.Cancel(b.Id)
	if err != nil || !changed {
		return view, err
	}
	// Its reminders here, as its event's deletion below finds the booking
	// already cancelled and stops (bookingEventDeleted).
	cancelGuestReminders(ctx, p.UserId, b.Id)
	if b.EventUUID != nil {
		du, err := userDomain.GetDgraphUserInfoByUUID(ctx, p.UserId.String())
		if err == nil && du != nil {
			owner := &model.UserInfo{UserPostgresInfo: model.User{Id: p.UserId}, UserDgraphInfo: *du}
			if err := DeleteEvent(ctx, *b.EventUUID, owner); err != nil {
				helpers.LogErrorWithContext(ctx, "business/Calendar/CancelBooking delete event err: %+v", err)
			}
		}
	}
	pageCache.forgetOwner(p.UserId.String())
	var hours WorkingHours
	_ = json.Unmarshal(p.Hours, &hours)
	view.OwnerTold = tellOwner(ctx, p, b, acceptedHostsOf(ctx, p), hours.TZ, b.StartsAt, b.EndsAt, notificationBusiness.BookingCancelled)
	// The guest added the invite, so the cancel takes it off their calendar.
	mailGuest(ctx, guestMail{
		to: b.GuestEmail, name: b.GuestName,
		subject:  fmt.Sprintf("Cancelled: %s with %s", p.Title, name),
		title:    "Your booking is cancelled",
		subtitle: p.Title,
		body:     FormatWhen(b.StartsAt, b.EndsAt, guestZone(b, p)),
		ctaText:  "Book another time", ctaURL: bookAgainURL(p, t.live),
		footer: "You cancelled this through your booking's link. If it's in your calendar, the update attached takes it off.",
		invite: bookingInvite(inviteCancel, b, p.Title, name, false, time.Now()),
	})
	return view, nil
}

// movable is whether a guest's link may move booking b: one still to come,
// not cancelled, with its meeting on the owner's calendar to move.
func movable(b *bookingModel.Booking, now time.Time) bool {
	return b != nil && b.CancelledAt == nil && b.EventUUID != nil && b.StartsAt.After(now)
}

// rescheduleHeld runs inside a move's page lock, after its slot check and
// before the move: nil but in the integration build's lock test.
var rescheduleHeld func()

// slotChecking runs as a booking's or a move's slot check starts, before the
// calendars are read: nil but in the integration build's lock test.
var slotChecking func()

// tellOwner tells page p's owner of a guest's change to booking b, at start
// to end in the page's zone tz: a cancel, or a move by the booking's link.
// hosts are the page's accepted hosts, told as its owner is: the caller's,
// who holds them already (a move) or reads them once (acceptedHostsOf).
func tellOwner(ctx context.Context, p *bookingModel.Page, b *bookingModel.Booking, hosts []string, tz string, start, end time.Time, change notificationBusiness.BookingChange) bool {
	event := ""
	if b.EventUUID != nil {
		event = b.EventUUID.String()
	}
	return notificationBusiness.TellBookingOwner(ctx, notificationBusiness.BookingNotice{
		OwnerUUID: p.UserId.String(), EventUUID: event, BookingID: b.Id.String(),
		GuestName: b.GuestName, PageTitle: p.Title, Start: start, End: end,
		TZ: tz, When: FormatWhen(start, end, tz), Change: change, HostUUIDs: hosts,
	})
}

// acceptedHostsOf is team page p's accepted hosts now, told of its guests'
// changes as its owner is; none for a solo page.
func acceptedHostsOf(ctx context.Context, p *bookingModel.Page) []string {
	if p.TeamMode != teamCollective {
		return nil
	}
	hosts, err := bookingModel.HostsOfPages(ctx, []uuid.UUID{p.Id})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar/acceptedHostsOf err: %+v", err)
	}
	return (&livePage{page: p, hosts: hosts}).hostIDs()
}

// guestMoveKey marks a context as a guest's move by their booking's link,
// with the booking's page: set only by RescheduleBooking around its change,
// read where bookingEventUpdated writes the guest's mail, so the one mail of
// a move says whose it was, and the page the move read isn't read again, and
// where legacyChange writes, so the move changes only the meeting it read.
type guestMoveKey struct{}

// guestMovePage is the page of the guest's move ctx is, nil for any other
// change.
func guestMovePage(ctx context.Context) *bookingModel.Page {
	p, _ := ctx.Value(guestMoveKey{}).(*bookingModel.Page)
	return p
}

// guestMoveOf is the page of the guest's move ctx is when b is a booking of
// that page, nil for any other change: another page's booking changed under
// the move reads its own page, as an owner's change does.
func guestMoveOf(ctx context.Context, b *bookingModel.Booking) *bookingModel.Page {
	if p := guestMovePage(ctx); p != nil && p.Id == b.PageId {
		return p
	}
	return nil
}

// RescheduleBooking moves a guest's booking, found by its link, to start: a
// free slot of its page by the page's rules (hours, notice, horizon, length),
// its own meeting left out of what's busy. A link that names no booking, a
// cancelled one or one begun, or one whose page takes no bookings answers
// ErrNoPage, as a missing booking does, and before any page or slot work but
// the page's. Its meeting is read once (eventToChange) and the calendars are
// read before the page's lock, Book's, as Book reads them before Reserve: no
// lock, nor its connection, waits on Google. Under the lock the page's own
// bookings are checked again, so nobody books the time meanwhile, and the
// meeting moves. The move is the page owner's own edit (UpdateEvent's
// change), marked as the guest's: bookingEventUpdated moves the
// booking with its meeting and sends the guest the one updated invite, in
// words of their own move. The page's owner is told as of a booking or a
// cancel (TellBookingOwner). A move to the time it has already changes
// nothing and tells nobody. A link moves its booking bookingMovesPerDay times
// a day (MovedTooOftenError); only a move about to change something counts.
func RescheduleBooking(ctx context.Context, token string, start, now time.Time) (*BookingView, error) {
	b, err := bookingModel.BookingByLink(helpers.LinkSecretHash(token))
	if err != nil {
		return nil, err
	}
	if !movable(b, now) {
		return nil, ErrNoPage
	}
	p, err := bookingModel.PageById(b.PageId)
	if err != nil {
		return nil, err
	}
	l, err := livePageOf(ctx, p)
	if err != nil {
		return nil, err
	}
	start, end, err := l.slotTimes(start, now)
	if err != nil {
		return nil, err
	}
	view := (&tokenBooking{b: b, p: p, owner: l.name, live: true}).view()
	if start.Equal(b.StartsAt) && end.Equal(b.EndsAt) {
		return view, nil
	}
	held, err := eventToChange(ctx, *b.EventUUID, l.owner)
	if errors.Is(err, ErrEventNotFound) {
		return nil, ErrNoPage
	}
	if err != nil {
		return nil, err
	}
	if err := l.slotFree(ctx, start, end, now, held.ev); err != nil {
		return nil, err
	}
	if lim := moveAllowed(ctx, b.Id.String()); !lim.Allowed {
		return nil, &MovedTooOftenError{Limit: lim}
	}
	moved := false
	err = bookingModel.HoldPage(ctx, p.Id, b.Id, start, end, func() error {
		if rescheduleHeld != nil {
			rescheduleHeld()
		}
		// The owner's own edit: the meeting keeps its title, notes and people.
		if err := held.change(context.WithValue(ctx, guestMoveKey{}, p), adapter.CreateOrUpdateEventInput{
			Title: held.ev.Title, Description: held.ev.Description,
			StartTime: start.Format(time.RFC3339), EndTime: end.Format(time.RFC3339),
		}, l.owner); err != nil {
			return err
		}
		moved = true
		return nil
	})
	var stale *StaleError
	switch {
	case errors.Is(err, bookingModel.ErrSlotTaken):
		return nil, &AvailabilityError{Msg: "That time was just taken. Pick another."}
	case errors.As(err, &stale):
		// The owner changed the meeting since the move read it: nothing
		// moved. Only a booking's own link gets here.
		return nil, &AvailabilityError{Msg: "Your booking just changed. Try again."}
	}
	if err != nil && !moved {
		return nil, err
	}
	if err != nil {
		// Moved, and only letting the lock go failed: the move stands.
		helpers.LogErrorWithContext(ctx, "business/Calendar/RescheduleBooking unlock err: %+v", err)
	}
	view.Start, view.End = start, end
	view.OwnerTold = tellOwner(ctx, p, b, l.hostIDs(), l.hours.TZ, start, end, notificationBusiness.BookingMoved)
	return view, nil
}

// guestMail is an email to a booking's guest. Every one carries the booking's
// calendar invite (invite.go), so the guest's calendar follows the booking.
type guestMail struct {
	to, name                 string
	subject, title, subtitle string
	body                     string // plain text
	ctaText, ctaURL          string // no URL, no button
	footer                   string
	invite                   invite
}

// mailGuest sends it where the server emails people outside the workspace
// (outsidersMailed: never on the public demo, whose visitors type any address
// into a booking page, nor on OneCamp Cloud's essentials-only tier; the page
// then shows the same details), and says whether it went.
func mailGuest(ctx context.Context, m guestMail) bool {
	if !outsidersMailed() {
		return false
	}
	htmlBody, text, err := emailService.Render(emailService.TemplateData{
		RecipientName: m.name,
		ActorName:     m.invite.Organizer.Name,
		Title:         m.title,
		Subtitle:      m.subtitle,
		Body:          m.body,
		CTAText:       m.ctaText,
		CTAURL:        m.ctaURL,
		BrandName:     "OneCamp",
		FooterNote:    m.footer,
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar/mailGuest render err: %+v", err)
		return false
	}
	err = sendGuestMail(ctx, emailService.SendOptions{
		From:        emailService.SenderAddress(),
		To:          m.to,
		Subject:     m.subject,
		HTML:        htmlBody,
		Text:        text,
		Attachments: []emailService.Attachment{m.invite.attachment()},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar/mailGuest send err: %+v", err)
		return false
	}
	return true
}

// cancelURL is a booking's link for its guest: it shows the booking and
// cancels it.
func cancelURL(token string) string {
	return authService.FrontendBaseURL() + "/booking/" + token
}

// bookAgainURL is where a guest books another time: the page, while it takes
// bookings; "" when it doesn't.
func bookAgainURL(p *bookingModel.Page, live bool) string {
	if !live {
		return ""
	}
	return authService.FrontendBaseURL() + "/book/" + p.Slug
}
