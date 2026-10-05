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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	model "github.com/akashc777/OneCamp/models/postgres/User"
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
}

var slugJunk = regexp.MustCompile(`[^a-z0-9]+`)

// NormaliseSlug turns what someone typed into a page address: lower case,
// letters and digits joined by single dashes. Pure.
func NormaliseSlug(s string) string {
	return strings.Trim(slugJunk.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-"), "-")
}

// CheckPage validates a page and returns it ready to store. Pure.
func CheckPage(in PageInput) (bookingModel.Page, error) {
	var p bookingModel.Page
	p.Slug = NormaliseSlug(in.Slug)
	if n := len(p.Slug); n < 3 || n > 40 {
		return p, &AvailabilityError{"Use 3 to 40 letters, digits or dashes for the address."}
	}
	p.Title = strings.Join(strings.Fields(in.Title), " ")
	if p.Title == "" || utf8.RuneCountInString(p.Title) > 80 {
		return p, &AvailabilityError{"Give the page a title of up to 80 characters."}
	}
	p.Description = strings.TrimSpace(in.Description)
	if utf8.RuneCountInString(p.Description) > 1000 {
		return p, &AvailabilityError{"Keep the description under 1,000 characters."}
	}
	if in.DurationMinutes < 10 || in.DurationMinutes > 480 {
		return p, &AvailabilityError{"Meetings run from 10 minutes to 8 hours."}
	}
	if in.BufferMinutes < 0 || in.BufferMinutes > 240 {
		return p, &AvailabilityError{"Keep the buffer between 0 and 4 hours."}
	}
	if in.MinNoticeMinutes < 0 || in.MinNoticeMinutes > 30*24*60 {
		return p, &AvailabilityError{"Notice can be up to 30 days."}
	}
	if in.MaxDaysAhead < 1 || in.MaxDaysAhead > 180 {
		return p, &AvailabilityError{"Let people book 1 to 180 days ahead."}
	}
	if _, err := in.Hours.Check(); err != nil {
		return p, err
	}
	hours, _ := json.Marshal(in.Hours)
	p.Hours = hours
	p.DurationMinutes, p.BufferMinutes = in.DurationMinutes, in.BufferMinutes
	p.MinNoticeMinutes, p.MaxDaysAhead, p.Active = in.MinNoticeMinutes, in.MaxDaysAhead, in.Active
	if in.Id != "" {
		id, err := uuid.Parse(in.Id)
		if err != nil {
			return p, &AvailabilityError{"That isn't one of your pages."}
		}
		p.Id = id
	}
	return p, nil
}

// SaveBookingPage creates or updates one of the person's pages.
func SaveBookingPage(ctx context.Context, owner uuid.UUID, in PageInput) (*bookingModel.Page, error) {
	p, err := CheckPage(in)
	if err != nil {
		return nil, err
	}
	p.UserId = owner
	if p.Id == uuid.Nil {
		if n, err := bookingModel.CountPages(owner); err != nil {
			return nil, err
		} else if n >= maxPagesPerPerson {
			return nil, &AvailabilityError{fmt.Sprintf("You have %d booking pages. Delete one to make another.", n)}
		}
	}
	saved, err := bookingModel.SavePage(p)
	if errors.Is(err, bookingModel.ErrSlugTaken) {
		return nil, &AvailabilityError{"Someone already uses that address. Try another."}
	}
	if err == nil && saved == nil {
		return nil, &AvailabilityError{"That isn't one of your pages."}
	}
	return saved, err
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
}

// ErrNoPage is a page that doesn't exist or is switched off; both answer
// the same, so a closed page reveals nothing.
var ErrNoPage = errors.New("no such booking page")

type livePage struct {
	page  *bookingModel.Page
	hours WorkingHours
	owner *model.UserInfo
	name  string
}

func loadPage(ctx context.Context, slug string) (*livePage, error) {
	p, err := bookingModel.PageBySlug(NormaliseSlug(slug))
	if err != nil {
		return nil, err
	}
	if p == nil || !p.Active {
		return nil, ErrNoPage
	}
	var hours WorkingHours
	if json.Unmarshal(p.Hours, &hours) != nil {
		return nil, ErrNoPage
	}
	du, err := userDomain.GetDgraphUserInfoByUUID(ctx, p.UserId.String())
	// Someone who has left the workspace takes no more bookings.
	if err != nil || du == nil || helpers.IsSoftDeleted(du.DeletedAt) {
		return nil, ErrNoPage
	}
	name := du.UserFullName
	if name == "" {
		name = du.UserName
	}
	return &livePage{page: p, hours: hours, name: name,
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

func (l *livePage) slots(ctx context.Context, from, to, now time.Time) ([]Interval, error) {
	busy, err := BusyFor(ctx, l.page.UserId.String(), from.Add(-24*time.Hour), to.Add(24*time.Hour))
	if err != nil {
		return nil, err
	}
	return FreeSlots(busy, from, to, l.hours, l.rules(now)), nil
}

// GetPublicPage is a page and its free slots between from and to (clamped
// to now, the page's horizon and a month).
func GetPublicPage(ctx context.Context, slug string, from, to, now time.Time) (*PublicPage, error) {
	l, err := loadPage(ctx, slug)
	if err != nil {
		return nil, err
	}
	if from.Before(now) {
		from = now
	}
	until := l.horizon(now)
	if to.After(until) {
		to = until
	}
	if to.Sub(from) > maxPublicRange {
		to = from.Add(maxPublicRange)
	}
	out := &PublicPage{
		Slug: l.page.Slug, Title: l.page.Title, Description: l.page.Description,
		DurationMinutes: l.page.DurationMinutes, OwnerName: l.name, OwnerTZ: l.hours.TZ,
		Slots: []Interval{}, BookableUntil: until,
	}
	if to.After(from) {
		slots, err := l.slots(ctx, from, to, now)
		if err != nil {
			return nil, err
		}
		if slots != nil {
			out.Slots = slots
		}
	}
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
}

// Booked is what the visitor gets back.
type Booked struct {
	Title       string    `json:"title"`
	OwnerName   string    `json:"owner_name"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	CancelToken string    `json:"cancel_token"`
	// Emailed is whether a confirmation went to the visitor; when not, the
	// page offers the calendar file and the cancel link itself.
	Emailed bool `json:"emailed"`
}

// CheckGuest tidies and validates a visitor's details. Pure.
func CheckGuest(in BookInput) (BookInput, error) {
	in.Name = strings.Join(strings.Fields(in.Name), " ")
	if in.Name == "" || utf8.RuneCountInString(in.Name) > 100 {
		return in, &AvailabilityError{"Enter your name."}
	}
	addr, err := mail.ParseAddress(strings.TrimSpace(in.Email))
	if err != nil || len(addr.Address) > 254 || !strings.Contains(addr.Address[strings.LastIndex(addr.Address, "@")+1:], ".") {
		return in, &AvailabilityError{"Enter an email address like name@example.com."}
	}
	in.Email = addr.Address
	in.Note = strings.TrimSpace(in.Note)
	if utf8.RuneCountInString(in.Note) > 1000 {
		return in, &AvailabilityError{"Keep the note under 1,000 characters."}
	}
	if _, err := time.LoadLocation(in.TZ); err != nil || in.TZ == "" {
		in.TZ = "UTC"
	}
	return in, nil
}

func newToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
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
	if n, err := bookingModel.CountRecentByEmail(l.page.Id, in.Email, now.Add(-24*time.Hour)); err != nil {
		return nil, err
	} else if n >= maxBookingsPerEmail {
		return nil, &AvailabilityError{"You've booked this page a few times today. Try again tomorrow."}
	}

	dur := time.Duration(l.page.DurationMinutes) * time.Minute
	start := in.Start.UTC().Truncate(time.Minute)
	end := start.Add(dur)
	if end.After(l.horizon(now)) {
		return nil, &AvailabilityError{"That time is too far ahead. Pick another."}
	}
	slots, err := l.slots(ctx, start.Add(-time.Hour), end.Add(time.Hour), now)
	if err != nil {
		return nil, err
	}
	free := false
	for _, s := range slots {
		if s.Start.Equal(start) {
			free = true
			break
		}
	}
	if !free {
		return nil, &AvailabilityError{"That time was just taken. Pick another."}
	}

	token, err := newToken()
	if err != nil {
		return nil, err
	}
	b, err := bookingModel.Reserve(bookingModel.Booking{
		PageId: l.page.Id, GuestName: in.Name, GuestEmail: in.Email, Note: in.Note, StartsAt: start, EndsAt: end,
	}, token)
	if errors.Is(err, bookingModel.ErrSlotTaken) {
		return nil, &AvailabilityError{"That time was just taken. Pick another."}
	}
	if err != nil {
		return nil, err
	}

	desc := fmt.Sprintf("Booked through your page “%s” by %s (%s).", l.page.Title, in.Name, in.Email)
	if in.Note != "" {
		desc += "\n\n" + in.Note
	}
	ev, err := CreateEvent(ctx, l.owner, adapter.CreateOrUpdateEventInput{
		Title:                fmt.Sprintf("%s with %s", l.page.Title, in.Name),
		Description:          desc,
		StartTime:            start.Format(time.RFC3339),
		EndTime:              end.Format(time.RFC3339),
		SyncToGoogleCalendar: true,
	})
	if err != nil {
		_ = bookingModel.Forget(b.Id)
		return nil, err
	}
	if id, err := uuid.Parse(ev.EventUuid); err == nil {
		if err := bookingModel.SetEvent(b.Id, id); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar/Book link event err: %+v", err)
		}
	}

	notificationBusiness.DispatchCalendarBooking(l.page.UserId.String(), in.Name, l.page.Title,
		FormatWhen(start, end, l.hours.TZ), b.Id.String(), false)
	emailed := sendGuestEmail(ctx, in, l, start, end, token, false)
	return &Booked{Title: l.page.Title, OwnerName: l.name, Start: start, End: end, CancelToken: token, Emailed: emailed}, nil
}

// BookingView is what a cancel link shows.
type BookingView struct {
	Title     string    `json:"title"`
	OwnerName string    `json:"owner_name"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Cancelled bool      `json:"cancelled"`
	Slug      string    `json:"slug"`
}

func bookingForToken(ctx context.Context, token string) (*bookingModel.Booking, *bookingModel.Page, string, error) {
	if len(token) != 48 {
		return nil, nil, "", ErrNoPage
	}
	b, err := bookingModel.BookingByToken(token)
	if err != nil {
		return nil, nil, "", err
	}
	if b == nil {
		return nil, nil, "", ErrNoPage
	}
	p, err := bookingModel.PageById(b.PageId)
	if err != nil || p == nil {
		return nil, nil, "", ErrNoPage
	}
	name := ""
	if du, err := userDomain.GetDgraphUserInfoByUUID(ctx, p.UserId.String()); err == nil && du != nil {
		name = du.UserFullName
		if name == "" {
			name = du.UserName
		}
	}
	return b, p, name, nil
}

// GetBooking is the booking behind a cancel link.
func GetBooking(ctx context.Context, token string) (*BookingView, error) {
	b, p, name, err := bookingForToken(ctx, token)
	if err != nil {
		return nil, err
	}
	return &BookingView{Title: p.Title, OwnerName: name, Start: b.StartsAt, End: b.EndsAt, Cancelled: b.CancelledAt != nil, Slug: p.Slug}, nil
}

// CancelBooking cancels through the guest's link: the event comes off the
// owner's calendar and they hear about it. Cancelling twice is not an error.
func CancelBooking(ctx context.Context, token string) (*BookingView, error) {
	b, p, name, err := bookingForToken(ctx, token)
	if err != nil {
		return nil, err
	}
	view := &BookingView{Title: p.Title, OwnerName: name, Start: b.StartsAt, End: b.EndsAt, Cancelled: true, Slug: p.Slug}
	changed, err := bookingModel.Cancel(b.Id)
	if err != nil || !changed {
		return view, err
	}
	if b.EventUUID != nil {
		du, err := userDomain.GetDgraphUserInfoByUUID(ctx, p.UserId.String())
		if err == nil && du != nil {
			owner := &model.UserInfo{UserPostgresInfo: model.User{Id: p.UserId}, UserDgraphInfo: *du}
			if err := DeleteEvent(ctx, *b.EventUUID, owner); err != nil {
				helpers.LogErrorWithContext(ctx, "business/Calendar/CancelBooking delete event err: %+v", err)
			}
		}
	}
	var hours WorkingHours
	_ = json.Unmarshal(p.Hours, &hours)
	notificationBusiness.DispatchCalendarBooking(p.UserId.String(), b.GuestName, p.Title,
		FormatWhen(b.StartsAt, b.EndsAt, hours.TZ), b.Id.String(), true)
	return view, nil
}

// sendGuestEmail confirms a booking to the visitor where the workspace sends
// notification email; workspaces on the essentials-only tier don't, and the
// page shows the same details instead.
func sendGuestEmail(ctx context.Context, in BookInput, l *livePage, start, end time.Time, token string, cancelled bool) bool {
	if !emailService.NotificationEmailEnabled() {
		return false
	}
	cancelURL := authService.FrontendBaseURL() + "/booking/" + token
	htmlBody, text, err := emailService.Render(emailService.TemplateData{
		RecipientName: in.Name,
		ActorName:     l.name,
		Title:         fmt.Sprintf("You're booked with %s", l.name),
		Subtitle:      l.page.Title,
		Body:          html.EscapeString(FormatWhen(start, end, in.TZ)),
		CTAText:       "Cancel this booking",
		CTAURL:        cancelURL,
		BrandName:     "OneCamp",
		FooterNote:    "You booked this time through a OneCamp booking page.",
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar/sendGuestEmail render err: %+v", err)
		return false
	}
	_, err = emailService.SendEmailWithOptions(ctx, emailService.SendOptions{
		From:    emailService.SenderAddress(),
		To:      in.Email,
		Subject: fmt.Sprintf("Booked: %s with %s", l.page.Title, l.name),
		HTML:    htmlBody,
		Text:    text,
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar/sendGuestEmail send err: %+v", err)
		return false
	}
	return true
}
