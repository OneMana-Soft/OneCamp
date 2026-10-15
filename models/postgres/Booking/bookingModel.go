package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Page is a booking page (migration 177).
type Page struct {
	Id               uuid.UUID       `json:"id"`
	UserId           uuid.UUID       `json:"user_id"`
	Slug             string          `json:"slug"`
	Title            string          `json:"title"`
	Description      string          `json:"description"`
	DurationMinutes  int             `json:"duration_minutes"`
	Hours            json.RawMessage `json:"hours"`
	BufferMinutes    int             `json:"buffer_minutes"`
	MinNoticeMinutes int             `json:"min_notice_minutes"`
	MaxDaysAhead     int             `json:"max_days_ahead"`
	Active           bool            `json:"active"`
	UpdatedAt        time.Time       `json:"updated_at"`
	// UpcomingBookings is how many active bookings are still to start, read
	// for the owner's list only: deleting the page says what becomes of them.
	UpcomingBookings int `json:"upcoming_bookings"`
}

// Booking is one slot a guest booked.
type Booking struct {
	Id          uuid.UUID  `json:"id"`
	PageId      uuid.UUID  `json:"page_id"`
	EventUUID   *uuid.UUID `json:"event_uuid,omitempty"`
	GuestName   string     `json:"guest_name"`
	GuestEmail  string     `json:"guest_email"`
	Note        string     `json:"note"`
	StartsAt    time.Time  `json:"starts_at"`
	EndsAt      time.Time  `json:"ends_at"`
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	// CancelToken is the guest's link: theirs alone, never sent anywhere else.
	CancelToken string `json:"-"`
}

// ErrSlotTaken is a slot someone else booked first.
var ErrSlotTaken = errors.New("slot taken")

// ErrSlugTaken is a page address another page already uses.
var ErrSlugTaken = errors.New("slug taken")

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
}

const pageColumns = `id, user_id, slug, title, description, duration_minutes, hours,
	buffer_minutes, min_notice_minutes, max_days_ahead, active, updated_at`

func scanPage(row interface{ Scan(...any) error }) (*Page, error) {
	var p Page
	err := row.Scan(&p.Id, &p.UserId, &p.Slug, &p.Title, &p.Description, &p.DurationMinutes, &p.Hours,
		&p.BufferMinutes, &p.MinNoticeMinutes, &p.MaxDaysAhead, &p.Active, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

// upcomingOfPage counts a page's active bookings still to start; it reads the
// outer query's booking_pages row.
const upcomingOfPage = `(SELECT COUNT(*) FROM bookings b
	WHERE b.page_id = booking_pages.id AND b.cancelled_at IS NULL AND b.starts_at > NOW())`

// ListPages returns a person's pages, newest first, each with how many
// bookings are still to come.
func ListPages(userID uuid.UUID) ([]Page, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx,
		`SELECT `+pageColumns+`, `+upcomingOfPage+` FROM booking_pages WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Page{}
	for rows.Next() {
		// scanPage's columns, then the count.
		var p Page
		if err := rows.Scan(&p.Id, &p.UserId, &p.Slug, &p.Title, &p.Description, &p.DurationMinutes, &p.Hours,
			&p.BufferMinutes, &p.MinNoticeMinutes, &p.MaxDaysAhead, &p.Active, &p.UpdatedAt,
			&p.UpcomingBookings); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountPages is how many pages a person has.
func CountPages(userID uuid.UUID) (int, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	var n int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM booking_pages WHERE user_id = $1`, userID).Scan(&n)
	return n, err
}

// PageBySlug is a page by its address, or nil.
func PageBySlug(slug string) (*Page, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scanPage(postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT `+pageColumns+` FROM booking_pages WHERE slug = $1`, slug))
}

// SavePage creates a page (Id zero) or updates one of the person's own.
// Returns nil when the page to update isn't theirs.
func SavePage(p Page) (*Page, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	var row *sql.Row
	if p.Id == uuid.Nil {
		row = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
			INSERT INTO booking_pages (user_id, slug, title, description, duration_minutes, hours,
			       buffer_minutes, min_notice_minutes, max_days_ahead, active)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING `+pageColumns,
			p.UserId, p.Slug, p.Title, p.Description, p.DurationMinutes, []byte(p.Hours),
			p.BufferMinutes, p.MinNoticeMinutes, p.MaxDaysAhead, p.Active)
	} else {
		row = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
			UPDATE booking_pages SET slug = $3, title = $4, description = $5, duration_minutes = $6,
			       hours = $7, buffer_minutes = $8, min_notice_minutes = $9, max_days_ahead = $10,
			       active = $11, updated_at = NOW()
			 WHERE id = $1 AND user_id = $2
			RETURNING `+pageColumns,
			p.Id, p.UserId, p.Slug, p.Title, p.Description, p.DurationMinutes, []byte(p.Hours),
			p.BufferMinutes, p.MinNoticeMinutes, p.MaxDaysAhead, p.Active)
	}
	saved, err := scanPage(row)
	if err != nil && helpers.IsUniqueViolation(err) {
		return nil, ErrSlugTaken
	}
	return saved, err
}

// DeletePage removes one of the person's pages, unless bookings made through
// it are still to come: removing the page removes its bookings (migration
// 177's cascade), and with them their guests' links. It answers whether it
// was removed and, when it wasn't for that, how many are still to come; false
// with 0 when the page isn't theirs. It holds Reserve's per-page lock, so no
// booking lands between the count and the delete.
func DeletePage(userID, id uuid.UUID) (bool, int, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "booking:"+id.String()); err != nil {
		return false, 0, err
	}
	var upcoming int
	err = tx.QueryRowContext(ctx, `SELECT `+upcomingOfPage+` FROM booking_pages WHERE id = $1 AND user_id = $2`,
		id, userID).Scan(&upcoming)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if upcoming > 0 {
		return false, upcoming, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM booking_pages WHERE id = $1 AND user_id = $2`, id, userID); err != nil {
		return false, 0, err
	}
	return true, 0, tx.Commit()
}

// Reserve records a booking unless it overlaps an active one on the page,
// holding a per-page lock so two guests can't take overlapping slots at once.
func Reserve(b Booking, cancelToken string) (*Booking, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "booking:"+b.PageId.String()); err != nil {
		return nil, err
	}
	var clash bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM bookings WHERE page_id = $1 AND cancelled_at IS NULL
		                 AND starts_at < $3 AND ends_at > $2)`, b.PageId, b.StartsAt, b.EndsAt).Scan(&clash); err != nil {
		return nil, err
	}
	if clash {
		return nil, ErrSlotTaken
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO bookings (page_id, guest_name, guest_email, note, starts_at, ends_at, cancel_token)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		b.PageId, b.GuestName, b.GuestEmail, b.Note, b.StartsAt, b.EndsAt, cancelToken).Scan(&b.Id); err != nil {
		return nil, err
	}
	return &b, tx.Commit()
}

// SetEvent links a booking to the event it made.
func SetEvent(id, eventUUID uuid.UUID) error {
	ctx, cancel := withTimeout()
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `UPDATE bookings SET event_uuid = $2 WHERE id = $1`, id, eventUUID)
	return err
}

// Forget removes a booking whose event could not be made.
func Forget(id uuid.UUID) error {
	ctx, cancel := withTimeout()
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `DELETE FROM bookings WHERE id = $1`, id)
	return err
}

const bookingColumns = `id, page_id, event_uuid, guest_name, guest_email, note, starts_at, ends_at, cancelled_at, created_at, cancel_token`

func scanBooking(row interface{ Scan(...any) error }) (*Booking, error) {
	var b Booking
	err := row.Scan(&b.Id, &b.PageId, &b.EventUUID, &b.GuestName, &b.GuestEmail, &b.Note, &b.StartsAt, &b.EndsAt,
		&b.CancelledAt, &b.CreatedAt, &b.CancelToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &b, err
}

// CancelByEvent cancels the booking that made an event, for its host deleting
// the event, and returns it: nil when the event made none, or its booking was
// cancelled already or has ended by now (a meeting that happened stays as it
// was).
func CancelByEvent(eventUUID uuid.UUID, now time.Time) (*Booking, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scanBooking(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		UPDATE bookings SET cancelled_at = NOW()
		 WHERE event_uuid = $1 AND cancelled_at IS NULL AND ends_at > $2
		RETURNING `+bookingColumns, eventUUID, now))
}

// MoveByEvent moves the active booking that made an event to the event's new
// time, and returns it with the time it had: nil when the event made none or
// its time didn't change.
func MoveByEvent(eventUUID uuid.UUID, start, end time.Time) (*Booking, time.Time, time.Time, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	var b Booking
	var wasStart, wasEnd time.Time
	// scanBooking's columns, then the time it had.
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		WITH was AS (
			SELECT id, starts_at, ends_at FROM bookings
			 WHERE event_uuid = $1 AND cancelled_at IS NULL AND (starts_at <> $2 OR ends_at <> $3)
			 FOR UPDATE)
		UPDATE bookings b SET starts_at = $2, ends_at = $3 FROM was WHERE b.id = was.id
		RETURNING b.id, b.page_id, b.event_uuid, b.guest_name, b.guest_email, b.note, b.starts_at, b.ends_at,
		          b.cancelled_at, b.created_at, b.cancel_token, was.starts_at, was.ends_at`,
		eventUUID, start, end).Scan(&b.Id, &b.PageId, &b.EventUUID, &b.GuestName, &b.GuestEmail, &b.Note, &b.StartsAt, &b.EndsAt,
		&b.CancelledAt, &b.CreatedAt, &b.CancelToken,
		&wasStart, &wasEnd)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, time.Time{}, err
	}
	return &b, wasStart, wasEnd, nil
}

// BookedEvent is an event a guest booked: who, and through which page.
type BookedEvent struct {
	EventUUID uuid.UUID
	GuestName string
	PageTitle string
}

// UpcomingBookedEvents are the events booked through the owner's pages whose
// bookings are active and haven't ended by now.
func UpcomingBookedEvents(owner uuid.UUID, now time.Time) ([]BookedEvent, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT b.event_uuid, b.guest_name, p.title
		  FROM bookings b JOIN booking_pages p ON p.id = b.page_id
		 WHERE p.user_id = $1 AND b.cancelled_at IS NULL AND b.event_uuid IS NOT NULL AND b.ends_at > $2`, owner, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BookedEvent
	for rows.Next() {
		var e BookedEvent
		if err := rows.Scan(&e.EventUUID, &e.GuestName, &e.PageTitle); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// BookingByToken is the booking a cancel link points at, or nil.
func BookingByToken(token string) (*Booking, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scanBooking(postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT `+bookingColumns+` FROM bookings WHERE cancel_token = $1`, token))
}

// Cancel marks a booking cancelled; false when it already was.
func Cancel(id uuid.UUID) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE bookings SET cancelled_at = NOW() WHERE id = $1 AND cancelled_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CountRecentByEmail is how many bookings an address made on a page lately:
// the spam brake beside the per-IP rate limit.
func CountRecentByEmail(pageID uuid.UUID, email string, since time.Time) (int, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	var n int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM bookings WHERE page_id = $1 AND lower(guest_email) = lower($2) AND created_at > $3`,
		pageID, email, since).Scan(&n)
	return n, err
}

// PageById is a page by id, or nil.
func PageById(id uuid.UUID) (*Page, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scanPage(postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT `+pageColumns+` FROM booking_pages WHERE id = $1`, id))
}
