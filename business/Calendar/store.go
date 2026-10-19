package Calendar

// Where the calendar's events are kept: an interface, so the service's rules
// are tested against a store in memory (service_test.go), and the Postgres
// one (models/postgres/CalendarV2) in the integration tests. Each write is one
// transaction: an event and its guests change together or not at all (CAL1).

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

type store interface {
	// personalCalendar is owner's own calendar, made with zone tz the first
	// time.
	personalCalendar(ctx context.Context, owner uuid.UUID, tz string) (uuid.UUID, error)
	// get is an event, in the trash or not; nil when there is none.
	get(ctx context.Context, id uuid.UUID) (*models.Event, error)
	// getMany is the events of these ids that exist.
	getMany(ctx context.Context, ids []uuid.UUID) ([]*models.Event, error)
	// attendees is each event's guests, in the order they were added.
	attendees(ctx context.Context, events []uuid.UUID) (map[uuid.UUID][]models.Attendee, error)
	// rangeOf is what person is on that could touch w, outside the trash.
	rangeOf(ctx context.Context, person uuid.UUID, w models.Window, limit int) ([]*models.Event, error)
	// trashOf is person's trash, newest first.
	trashOf(ctx context.Context, person uuid.UUID, before *time.Time, limit int) ([]*models.Event, error)
	// awayOf is these people's out-of-office events that could touch w.
	awayOf(ctx context.Context, people []uuid.UUID, w models.Window, limit int) ([]*models.Event, error)
	// zoneHints is the zones these people have given OneCamp.
	zoneHints(ctx context.Context, people []uuid.UUID) (map[uuid.UUID]models.ZoneHints, error)
	// known is which of these ids are people with an account row: the only
	// ones an event's guest list can hold.
	known(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error)
	// peopleByEmail is the accounts these addresses (in lower case) belong to.
	peopleByEmail(ctx context.Context, emails []string) (map[string]uuid.UUID, error)

	// create writes a new event and its guests.
	create(ctx context.Context, e *models.Event, guests []models.Attendee) error
	// update writes e and its guest changes when it is still at expect (any
	// sequence when nil), and answers its new sequence (models.ErrStale,
	// models.ErrNotFound). Guests taken off (people by id, guests by email
	// by address) take their answers for single instances with them.
	update(ctx context.Context, e *models.Event, expect *int, w guestWrites) (int, error)
	// setDeleted moves an event into the trash or out of it.
	setDeleted(ctx context.Context, id uuid.UUID, at *time.Time, by *uuid.UUID, now time.Time, expect *int) (int, error)
	// removeGuest takes person off an event, and says whether they were on it.
	removeGuest(ctx context.Context, event, person uuid.UUID, now time.Time) (bool, error)
	// setGoogleID records the event's copy in Google ("" forgets it).
	setGoogleID(ctx context.Context, id uuid.UUID, googleID string) error

	// answer writes a guest's answer and note to an event or a whole series,
	// clearing what they answered for single instances of it; or, with rid,
	// to the one instance of a series that starts there alone. It says
	// whether they are on it. An answer never moves the event's sequence.
	answer(ctx context.Context, event uuid.UUID, rid *time.Time, k models.GuestKey, response, note string, at time.Time) (bool, error)
	// instanceAnswers is the answers given for these instances alone: the
	// instance of each series[i] that starts at rids[i].
	instanceAnswers(ctx context.Context, series []uuid.UUID, rids []time.Time) ([]models.InstanceAnswer, error)
	// guestByLink is the guest by email an answer link's hash names, or nil.
	guestByLink(ctx context.Context, hash []byte) (*models.Attendee, error)
	// setInviteState records whether a guest by email was emailed their
	// invitation; setLink gives them a new answer link; setNudged stamps when
	// guests were last asked again for an answer.
	setInviteState(ctx context.Context, event uuid.UUID, email, state string) error
	setLink(ctx context.Context, event uuid.UUID, email string, hash []byte, sealed string) error
	setNudged(ctx context.Context, event uuid.UUID, people []uuid.UUID, emails []string, at time.Time) error

	// The overrides of series: those that could touch w, all of one
	// series', and the one of an instance (nil for none).
	overridesOf(ctx context.Context, series []uuid.UUID, w models.Window) ([]*models.Event, error)
	overridesOfSeries(ctx context.Context, series uuid.UUID) ([]*models.Event, error)
	overrideAt(ctx context.Context, series uuid.UUID, recurrence time.Time) (*models.Event, error)
	// apply writes a change to a series in one transaction: when the series
	// is still as its guard says, new rows first (an override may move onto a
	// new series), then the rows' updates (each at its sequence, when one is
	// named), their guests, and rows into the trash or out of it
	// (models.ErrStale, models.ErrNotFound).
	apply(ctx context.Context, b batch) error
}

// batch is a change to a series written at once.
type batch struct {
	// guard, when set, is what the series must still be.
	guard   *seriesGuard
	updates []rowUpdate
	inserts []*models.Event
	// guests are guests to add; drop, the people to take off each event,
	// and dropEmails the guests by email; roles, guests already on an
	// event who are now required or optional.
	guests     []models.Attendee
	drop       map[uuid.UUID][]uuid.UUID
	dropEmails map[uuid.UUID][]string
	roles      []models.Attendee
	// trash and restore move rows into the trash and out of it.
	trash, restore []uuid.UUID
	// folded are overrides whose changes this batch moves onto their series'
	// own row: nothing is left in them, and they go (never anything of a
	// person's: the trash keeps all of that).
	folded []uuid.UUID
	now    time.Time
	by     *uuid.UUID
}

// seriesGuard is what a series must still be, in the transaction, for a batch
// to be written: at sequence (when named) and with overrides overrides,
// trashed ones counted (when not negative). Its row is held until the end.
type seriesGuard struct {
	id        uuid.UUID
	sequence  *int
	overrides int
}

// rowUpdate is one row's new version, written when it is still at expect.
type rowUpdate struct {
	e      *models.Event
	expect *int
}

// maxOverrides is the most changed instances a series carries along when it
// changes as a whole.
const maxOverrides = 5000

// postgresStore keeps events in Postgres.
type postgresStore struct{}

func inTx(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	return postgresInit.InTx(ctx, postgresInit.DBConn.DBTimeout, fn)
}

func (postgresStore) personalCalendar(ctx context.Context, owner uuid.UUID, tz string) (uuid.UUID, error) {
	return models.PersonalCalendar(ctx, models.Pool(), owner, uuid.New(), tz)
}

func (postgresStore) get(ctx context.Context, id uuid.UUID) (*models.Event, error) {
	return models.Get(ctx, models.Pool(), id)
}

func (postgresStore) getMany(ctx context.Context, ids []uuid.UUID) ([]*models.Event, error) {
	return models.GetMany(ctx, models.Pool(), ids)
}

func (postgresStore) attendees(ctx context.Context, events []uuid.UUID) (map[uuid.UUID][]models.Attendee, error) {
	return models.AttendeesOf(ctx, models.Pool(), events)
}

func (postgresStore) rangeOf(ctx context.Context, person uuid.UUID, w models.Window, limit int) ([]*models.Event, error) {
	return models.RangeOf(ctx, models.Pool(), person, w, limit)
}

func (postgresStore) trashOf(ctx context.Context, person uuid.UUID, before *time.Time, limit int) ([]*models.Event, error) {
	return models.TrashOf(ctx, models.Pool(), person, before, limit)
}

func (postgresStore) awayOf(ctx context.Context, people []uuid.UUID, w models.Window, limit int) ([]*models.Event, error) {
	return models.AwayOf(ctx, models.Pool(), people, w, limit)
}

func (postgresStore) zoneHints(ctx context.Context, people []uuid.UUID) (map[uuid.UUID]models.ZoneHints, error) {
	return models.ZoneHintsOf(ctx, models.Pool(), people)
}

func (postgresStore) known(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	return models.PeopleIn(ctx, models.Pool(), ids)
}

func (postgresStore) peopleByEmail(ctx context.Context, emails []string) (map[string]uuid.UUID, error) {
	return models.PeopleByEmail(ctx, models.Pool(), emails)
}

func (postgresStore) create(ctx context.Context, e *models.Event, guests []models.Attendee) error {
	return inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := models.Insert(ctx, tx, e); err != nil {
			return err
		}
		return models.AddAttendees(ctx, tx, guests)
	})
}

// guestWrites is what one write does to an event's guests: who comes on,
// who goes off (people by id, guests by email by address), and who stays in
// another role.
type guestWrites struct {
	add          []models.Attendee
	remove       []uuid.UUID
	removeEmails []string
	roles        []models.Attendee
}

func (postgresStore) update(ctx context.Context, e *models.Event, expect *int, w guestWrites) (int, error) {
	var seq int
	err := inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		if seq, err = models.Update(ctx, tx, e, expect); err != nil {
			return err
		}
		if err := dropGuests(ctx, tx, e.ID, w.remove, w.removeEmails); err != nil {
			return err
		}
		if err := models.AddAttendees(ctx, tx, w.add); err != nil {
			return err
		}
		return models.SetRoles(ctx, tx, w.roles)
	})
	return seq, err
}

// dropGuests takes people and guests by email off an event, with what they
// answered for its instances alone.
func dropGuests(ctx context.Context, tx *sql.Tx, event uuid.UUID, people []uuid.UUID, emails []string) error {
	if _, err := models.RemoveAttendees(ctx, tx, event, people); err != nil {
		return err
	}
	if _, err := models.RemoveEmailGuests(ctx, tx, event, emails); err != nil {
		return err
	}
	return models.ForgetInstanceAnswers(ctx, tx, event, people, emails)
}

func (postgresStore) setDeleted(ctx context.Context, id uuid.UUID, at *time.Time, by *uuid.UUID, now time.Time, expect *int) (int, error) {
	return models.SetDeleted(ctx, models.Pool(), id, at, by, now, expect)
}

func (postgresStore) removeGuest(ctx context.Context, event, person uuid.UUID, now time.Time) (bool, error) {
	removed := false
	err := inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		n, err := models.RemoveAttendees(ctx, tx, event, []uuid.UUID{person})
		if err != nil || n == 0 {
			return err
		}
		removed = true
		if err := models.ForgetInstanceAnswers(ctx, tx, event, []uuid.UUID{person}, nil); err != nil {
			return err
		}
		_, err = models.Touch(ctx, tx, event, now)
		return err
	})
	return removed, err
}

func (postgresStore) setGoogleID(ctx context.Context, id uuid.UUID, googleID string) error {
	return models.SetGoogleID(ctx, models.Pool(), id, googleID)
}

func (postgresStore) overridesOf(ctx context.Context, series []uuid.UUID, w models.Window) ([]*models.Event, error) {
	return models.OverridesOf(ctx, models.Pool(), series, w)
}

func (postgresStore) overridesOfSeries(ctx context.Context, series uuid.UUID) ([]*models.Event, error) {
	return models.OverridesOfSeries(ctx, models.Pool(), series, maxOverrides)
}

func (postgresStore) overrideAt(ctx context.Context, series uuid.UUID, recurrence time.Time) (*models.Event, error) {
	return models.OverrideAt(ctx, models.Pool(), series, recurrence)
}

func (postgresStore) apply(ctx context.Context, b batch) error {
	return inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if g := b.guard; g != nil {
			seq, overrides, err := models.LockSeries(ctx, tx, g.id)
			if err != nil {
				return err
			}
			if (g.sequence != nil && seq != *g.sequence) || (g.overrides >= 0 && overrides != g.overrides) {
				return models.ErrStale
			}
		}
		for _, e := range b.inserts {
			if err := models.Insert(ctx, tx, e); err != nil {
				return err
			}
		}
		for _, u := range b.updates {
			if _, err := models.Update(ctx, tx, u.e, u.expect); err != nil {
				return err
			}
		}
		if err := models.AddAttendees(ctx, tx, b.guests); err != nil {
			return err
		}
		for event, people := range b.drop {
			if err := dropGuests(ctx, tx, event, people, nil); err != nil {
				return err
			}
		}
		for event, emails := range b.dropEmails {
			if err := dropGuests(ctx, tx, event, nil, emails); err != nil {
				return err
			}
		}
		if err := models.SetRoles(ctx, tx, b.roles); err != nil {
			return err
		}
		for _, id := range b.trash {
			if _, err := models.SetDeleted(ctx, tx, id, &b.now, b.by, b.now, nil); err != nil {
				return err
			}
		}
		for _, id := range b.restore {
			if _, err := models.SetDeleted(ctx, tx, id, nil, nil, b.now, nil); err != nil {
				return err
			}
		}
		return models.RemoveFoldedOverrides(ctx, tx, b.folded)
	})
}

func (postgresStore) answer(ctx context.Context, event uuid.UUID, rid *time.Time, k models.GuestKey, response, note string, at time.Time) (bool, error) {
	on := false
	err := inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		if rid == nil {
			if on, err = models.SetAnswer(ctx, tx, event, k, response, note, at); err != nil || !on {
				return err
			}
			return models.ClearInstanceAnswers(ctx, tx, event, k)
		}
		if on, err = models.GuestOn(ctx, tx, event, k); err != nil || !on {
			return err
		}
		a := models.InstanceAnswer{EventID: event, RecurrenceID: *rid, UserID: k.UserID, Email: k.Email, Response: response, Note: note, RespondedAt: at}
		return models.SetInstanceAnswer(ctx, tx, a)
	})
	return on, err
}

func (postgresStore) instanceAnswers(ctx context.Context, series []uuid.UUID, rids []time.Time) ([]models.InstanceAnswer, error) {
	return models.InstanceAnswersOf(ctx, models.Pool(), series, rids)
}

func (postgresStore) guestByLink(ctx context.Context, hash []byte) (*models.Attendee, error) {
	return models.AttendeeByLink(ctx, models.Pool(), hash)
}

func (postgresStore) setInviteState(ctx context.Context, event uuid.UUID, email, state string) error {
	return models.SetInviteState(ctx, models.Pool(), event, email, state)
}

func (postgresStore) setLink(ctx context.Context, event uuid.UUID, email string, hash []byte, sealed string) error {
	return models.SetLink(ctx, models.Pool(), event, email, hash, sealed)
}

func (postgresStore) setNudged(ctx context.Context, event uuid.UUID, people []uuid.UUID, emails []string, at time.Time) error {
	return models.SetNudged(ctx, models.Pool(), event, people, emails, at)
}
