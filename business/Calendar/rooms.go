package Calendar

// Rooms (gcal-6 head 1, Resource Directory #47). Permission model: admins
// manage rooms (the one admin gate, /admin); members list them and add them
// to events they may edit; guests and bots see no directory and add no room
// (403); a room's bookings show only busy to anyone who may not see the
// event, the room's name included; a retired room keeps its past bookings and
// takes no new one; every delete is soft; every admin change is audited.
//
// The refusal: a room's calendar refuses a double booking at write time. The
// event's write transaction locks the rooms' rows (models.LockRooms, one
// order), then compares every occurrence of the event with every occurrence
// of the room's other bookings, each series expanded by expandInstances over
// the event's own span (checkWindow: from its start, or a day before now when
// later, to its last end; an endless series for awayHorizon, and on to the
// last end of any booking the rooms have further out). It fails closed: past
// maxRoomBookings or maxRoomOccurrences the write is refused
// (ErrRoomsTooMany), never let through unchecked.

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// KindRoom is a room's resource kind in the audit trail.
const KindRoom = "calendar_room"

// Room is a room as the API sends it.
type Room struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Building string   `json:"building"`
	Floor    string   `json:"floor"`
	Capacity int      `json:"capacity"`
	Features []string `json:"features"`
	Retired  bool     `json:"retired"`
}

// RoomInput is an admin's room: on a change, the fields sent.
type RoomInput struct {
	Name     *string   `json:"name"`
	Building *string   `json:"building"`
	Floor    *string   `json:"floor"`
	Capacity *int      `json:"capacity"`
	Features *[]string `json:"features"`
}

// RoomBooking is one booking on a room's calendar: when, and to someone who
// may see the event, which and its rooms; else busy alone.
type RoomBooking struct {
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Busy    bool      `json:"busy"`
	EventID string    `json:"event_id,omitempty"`
	Title   string    `json:"title,omitempty"`
	Rooms   []Room    `json:"rooms,omitempty"`
}

// RoomTakenError is a booking refused: the room is held then by another
// event (named only to a writer who may see it).
type RoomTakenError struct {
	RoomID string `json:"room_id"`
	Room   string `json:"room"`
	// At is where the refused occurrence first meets the other booking, and
	// Date its day in the refused event's zone (a clash on a long booking's
	// third day is that day).
	At      time.Time `json:"at"`
	Date    string    `json:"date"`
	EventID string    `json:"event_id,omitempty"`
	Title   string    `json:"title,omitempty"`
	other   uuid.UUID
	// occurrence: the refused write restores one occurrence of a series,
	// whose rooms are the series' (it never strips one from the series).
	occurrence bool
}

// roomsTooMany is ErrRoomsTooMany: its message is the answer, in words.
type roomsTooMany struct{}

func (roomsTooMany) Error() string {
	return "There's too much booked in that room to check it's free then. Book a shorter stretch, or fewer repeats."
}

func (e *RoomTakenError) Error() string {
	named := ""
	if e.Title != "" {
		named = " (" + e.Title + ")"
	}
	if e.occurrence {
		return e.Room + " is booked then" + named + ". Change the series' rooms or restore it later."
	}
	return e.Room + " is booked on " + e.Date + named + "."
}

// The clash check's caps: past either it can't see every clash, so the
// write is refused (errRoomsTooMany), never let through unchecked.
var (
	maxRoomBookings    = 5000 // other bookings on the rooms read in one check
	maxRoomOccurrences = 5000 // occurrences of one event expanded in one check
)

var (
	errRoomsForMembers = refusal("Only workspace members can see and book rooms.")
	// ErrNoRoom is a room that isn't there (404).
	ErrNoRoom = errors.New("calendar: no such room")
	// ErrRoomsTooMany is a write whose clash check would pass a cap (409
	// rooms_too_many): refused, as a clash past the cap would go unseen.
	ErrRoomsTooMany error = roomsTooMany{}
)

func meetingRoom(r models.Room) Room {
	f := r.Features
	if f == nil {
		f = []string{}
	}
	return Room{ID: r.ID.String(), Name: r.Name, Building: r.Building, Floor: r.Floor, Capacity: r.Capacity, Features: f, Retired: r.DeletedAt != nil}
}

// membersOnlyRooms refuses anyone but a workspace member (guests, bots).
func (s *service) membersOnlyRooms(ctx context.Context, person uuid.UUID) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	m, err := s.store.membersNow(ctx, []uuid.UUID{person})
	if err != nil {
		return err
	}
	if !m[person] {
		return errRoomsForMembers
	}
	return nil
}

// roomIDs reads ids; an unknown one is ErrNoRoom.
func roomIDs(ids []string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(ids))
	for _, s := range ids {
		id, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil {
			return nil, ErrNoRoom
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// ListRooms is every live room, for a member.
func ListRooms(ctx context.Context, who *model.UserInfo) ([]Room, error) {
	if err := stored.membersOnlyRooms(ctx, personOf(who)); err != nil {
		return nil, err
	}
	rows, err := models.LiveRooms(ctx, models.Pool())
	out := make([]Room, 0, len(rows))
	for _, r := range rows {
		out = append(out, meetingRoom(r))
	}
	return out, err
}

func applyRoom(r *models.Room, in RoomInput) error {
	if in.Name != nil {
		r.Name = strings.TrimSpace(*in.Name)
	}
	if in.Building != nil {
		r.Building = strings.TrimSpace(*in.Building)
	}
	if in.Floor != nil {
		r.Floor = strings.TrimSpace(*in.Floor)
	}
	if in.Capacity != nil {
		r.Capacity = *in.Capacity
	}
	if in.Features != nil {
		r.Features = []string{}
		for _, f := range *in.Features {
			if f = strings.TrimSpace(f); f != "" && len(f) <= 40 && !slices.Contains(r.Features, f) && len(r.Features) < 20 {
				r.Features = append(r.Features, f)
			}
		}
	}
	switch {
	case r.Name == "" || len(r.Name) > 120:
		return inputError("A room needs a name of up to 120 characters.")
	case len(r.Building) > 120 || len(r.Floor) > 40:
		return inputError("A building takes up to 120 characters and a floor up to 40.")
	case r.Capacity < 0 || r.Capacity > 10000:
		return inputError("A room's capacity is 0 to 10,000 people.")
	}
	return nil
}

// tellRoom audits an admin's change to a room in the calendar's trail.
func (s *service) tellRoom(ctx context.Context, who *model.UserInfo, kind resourceEventBusiness.Kind, id uuid.UUID) {
	s.emit(ctx, resourceEventBusiness.Event{Kind: kind, ResourceKind: KindRoom, ResourceUUID: id.String(),
		Actor: roomActor(who), At: s.now()})
}

// roomActor is who a room's change is audited as: the admin, or with no
// admin (the workspace adding a room itself) the server's own change, which
// names no one. Pure.
func roomActor(who *model.UserInfo) resourceEventBusiness.Actor {
	if p := personOf(who); p != uuid.Nil {
		return resourceEventBusiness.Actor{UserUUID: p.String()}
	}
	return resourceEventBusiness.Actor{System: true}
}

func roomWriteError(err error) error {
	if errors.Is(err, models.ErrNoRoom) {
		return ErrNoRoom
	}
	if helpers.IsUniqueViolationOn(err, "idx_calendar_rooms_live_name") {
		return inputError("There's a room by that name already.")
	}
	return err
}

// AddRoom is an admin adding a room (the router's admin gate decides who).
// A nil who is the workspace adding a room itself (the demo seed): its row
// and its audit name no one. Only internal callers pass nil; a controller
// passes the request's admin.
func AddRoom(ctx context.Context, who *model.UserInfo, in RoomInput) (*Room, error) {
	s := stored
	// Features start empty, not nil: the column is NOT NULL and pq.Array(nil) is NULL.
	r := models.Room{ID: uuid.New(), Features: []string{}}
	if err := applyRoom(&r, in); err != nil {
		return nil, err
	}
	if err := roomWriteError(models.AddRoom(ctx, r, personOf(who), s.now())); err != nil {
		return nil, err
	}
	s.tellRoom(ctx, who, resourceEventBusiness.CalendarRoomAdded, r.ID)
	out := meetingRoom(r)
	return &out, nil
}

func liveRoom(ctx context.Context, id string) (models.Room, error) {
	ids, err := roomIDs([]string{id})
	if err != nil {
		return models.Room{}, err
	}
	rows, err := models.RoomsByID(ctx, models.Pool(), ids)
	if err != nil {
		return models.Room{}, err
	}
	if len(rows) == 0 {
		return models.Room{}, ErrNoRoom
	}
	return rows[0], nil
}

// ChangeRoom is an admin changing a live room's fields.
func ChangeRoom(ctx context.Context, who *model.UserInfo, id string, in RoomInput) (*Room, error) {
	s := stored
	r, err := liveRoom(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := applyRoom(&r, in); err != nil {
		return nil, err
	}
	if err := roomWriteError(models.SetRoom(ctx, r, personOf(who), s.now())); err != nil {
		return nil, err
	}
	s.tellRoom(ctx, who, resourceEventBusiness.CalendarRoomChanged, r.ID)
	out := meetingRoom(r)
	return &out, nil
}

// RetireRoom is an admin retiring a room: it keeps its bookings and takes no
// new one (soft).
func RetireRoom(ctx context.Context, who *model.UserInfo, id string) (*Room, error) {
	s := stored
	r, err := liveRoom(ctx, id)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if err := roomWriteError(models.RetireRoom(ctx, r.ID, personOf(who), now)); err != nil {
		return nil, err
	}
	s.tellRoom(ctx, who, resourceEventBusiness.CalendarRoomRetired, r.ID)
	r.DeletedAt = &now
	out := meetingRoom(r)
	return &out, nil
}

// roomSpans is each room's booked spans in [from, to): every occurrence of
// every live booking, with the event that holds it.
// roomSpan is one occurrence of a booking: its span, the event (or series)
// that holds the room, and the occurrence's own title (an instance's may
// differ from its series').
type roomSpan struct {
	start, end time.Time
	event      *models.Event
	title      string
}

// roomCheck is one stretch a write is checked over: the bookings read for
// it, and their occurrences in it by room.
type roomCheck struct {
	w     models.Window
	rows  []*models.Event
	spans map[uuid.UUID][]roomSpan
}

// roomSpansIn is the checks a write over w makes of the rooms' bookings
// (never skip's); the first is their occurrences in w, by room. reach (an
// endless writer's check) also reads every booking ahead, and each that
// goes on past w is one more check over its own span past w (an endless one
// that begins past w: its first awayHorizon), so a booking past the horizon
// is still checked and a check costs as much as the bookings it compares,
// not the years between them. Past maxRoomBookings bookings, or
// maxRoomOccurrences occurrences of one in a check, it is ErrRoomsTooMany.
func roomSpansIn(ctx context.Context, db models.DB, rooms []uuid.UUID, w models.Window, skip uuid.UUID, reach bool) ([]roomCheck, error) {
	to := w.To
	if reach {
		to = w.From.AddDate(1000, 0, 0)
	}
	booked, err := models.BookingsOn(ctx, db, rooms, w.From, to, skip, maxRoomBookings+1)
	if err != nil {
		return nil, err
	}
	if len(booked) > maxRoomBookings {
		return nil, ErrRoomsTooMany
	}
	rows := make([]*models.Event, 0, len(booked))
	for _, b := range booked {
		if !slices.ContainsFunc(rows, func(e *models.Event) bool { return e.ID == b.Event.ID }) {
			rows = append(rows, b.Event)
		}
	}
	checks := []roomCheck{{w: w, rows: rows}}
	for _, e := range rows {
		from, end := bookingSpan(e)
		if end == nil && from.After(w.To) {
			last := from.Add(awayHorizon)
			end = &last
		}
		if !reach || end == nil || !end.After(w.To) {
			continue
		}
		if from.Before(w.To) {
			from = w.To
		}
		checks = append(checks, roomCheck{w: spanWindow(from, *end), rows: []*models.Event{e}})
	}
	overrides, err := overridesRead(ctx, rows, checksSpan(checks), func(ctx context.Context, ids []uuid.UUID, w models.Window) ([]*models.Event, error) {
		return models.OverridesOf(ctx, db, ids, w)
	})
	if err != nil {
		return nil, err
	}
	for i := range checks {
		c := &checks[i]
		c.spans = map[uuid.UUID][]roomSpan{}
		for _, row := range c.rows {
			insts, err := checkedInstances(ctx, row, overrides, c.w)
			if err != nil {
				return nil, err
			}
			for _, b := range booked {
				if b.Event.ID != row.ID {
					continue
				}
				for _, in := range insts {
					s, e := spanOf(in.Event)
					c.spans[b.RoomID] = append(c.spans[b.RoomID], roomSpan{s, e, b.Event, in.Event.Title})
				}
			}
		}
	}
	return checks, nil
}

// roomSpansOver is the rooms' bookings' occurrences in w, by room: a read
// makes the one check.
func roomSpansOver(ctx context.Context, rooms []uuid.UUID, w models.Window) (map[uuid.UUID][]roomSpan, error) {
	checks, err := roomSpansIn(ctx, models.Pool(), rooms, w, uuid.Nil, false)
	if err != nil {
		return nil, err
	}
	return checks[0].spans, nil
}

// checksSpan is the stretch the checks cover together: the first's start to
// the farthest end. Pure.
func checksSpan(checks []roomCheck) models.Window {
	w := checks[0].w
	for _, c := range checks[1:] {
		if c.w.To.After(w.To) {
			w = spanWindow(w.From, c.w.To)
		}
	}
	return w
}

// checkedInstances is e's occurrences in w for a clash check: past
// maxRoomOccurrences it is ErrRoomsTooMany, as the rest would go unchecked.
func checkedInstances(ctx context.Context, e *models.Event, overrides seriesOverrides, w models.Window) ([]instance, error) {
	insts := expandInstances(ctx, []*models.Event{e}, overrides, w, maxRoomOccurrences+1)
	if len(insts) > maxRoomOccurrences {
		return nil, ErrRoomsTooMany
	}
	return insts, nil
}

// bookingSpan is the stretch an event's bookings cover: its first start to
// its last end (nil: a series without an end), the last instance placed as
// the series places it (an all-day one's dates in its own zone). Pure.
func bookingSpan(e *models.Event) (time.Time, *time.Time) {
	start, end := spanOf(e)
	if e.RRule == "" {
		return start, &end
	}
	if e.UntilAt != nil {
		_, last := spanOf(madeInstance(e, *e.UntilAt).Event)
		return start, &last
	}
	return start, nil
}

// checkWindow is how far a write looks for clashes: the event's own span,
// from its start (or a day before now, when later, so a series begun long
// ago is still checked) to its last end; an endless series for awayHorizon
// (roomSpansIn reaches further for a booking ahead). Pure.
func checkWindow(e *models.Event, now time.Time) models.Window {
	from, end := bookingSpan(e)
	if day := now.Add(-24 * time.Hour); day.After(from) {
		from = day
	}
	to := from.Add(awayHorizon)
	if end != nil {
		to = *end
	}
	if to.Before(from) {
		to = from
	}
	return spanWindow(from, to)
}

// reachOverrides widens w over live overrides: an instance moved past its
// series' last end is still checked. Pure.
func reachOverrides(w models.Window, ovs map[int64]*models.Event) models.Window {
	to := w.To
	for _, o := range ovs {
		if _, e := spanOf(o); live(o) && e.After(to) {
			to = e
		}
	}
	if to.Equal(w.To) {
		return w
	}
	return spanWindow(w.From, to)
}

// roomsAsk is what a write asks of rooms, carried to its transaction on the
// write's own ctx (askRooms makes it for that one write). The store's write
// methods take no rooms argument, so the ask rides the ctx: a caller must
// never reuse an asked ctx for a second write, which would inherit the ask.
type roomsAsk struct {
	rooms []uuid.UUID
	set   bool
	by    uuid.UUID
}

type roomsAskKey struct{}

func withRooms(ctx context.Context, rooms []uuid.UUID, by uuid.UUID) context.Context {
	return context.WithValue(ctx, roomsAskKey{}, roomsAsk{rooms: rooms, set: true, by: by})
}

// holdRoomsIn runs inside a write's transaction once its rows are written
// (series: the series a series' batch is for, else uuid.Nil). The rooms of
// every event and series the rows belong to are held again with their
// occurrences as written, so a write that moves one onto another booking is
// refused; a whole event's restore (letGo) instead gives up a room booked
// while it was in the trash (the service lists those it let go). Every room is locked first,
// all of them in one order. The loop is one pass per room-holding head (a
// single write: the row and, for "this and following", its split series; a
// batch: each room-holding row in it, at most one Google sync page).
func holdRoomsIn(ctx context.Context, tx *sql.Tx, rows []*models.Event, series uuid.UUID, now time.Time, letGo bool) error {
	heads, err := roomHolders(ctx, tx, rows)
	if err != nil || len(heads) == 0 {
		return err
	}
	held, err := models.RoomsHeldBy(ctx, tx, idsOf(heads))
	if err != nil {
		return err
	}
	ask, _ := ctx.Value(roomsAskKey{}).(roomsAsk)
	plan, err := planHolds(heads, idsOf(rows), held, series, ask)
	if err != nil || len(plan) == 0 {
		return err
	}
	var all []uuid.UUID
	for _, h := range plan {
		all = unionRooms(all, h.rooms)
	}
	locked, err := models.LockRooms(ctx, tx, all)
	if err != nil {
		return err
	}
	if len(locked) != len(all) {
		return ErrNoRoom
	}
	rooms := make(map[uuid.UUID]models.Room, len(locked))
	for _, r := range locked {
		rooms[r.ID] = r
	}
	for _, h := range plan {
		if err := holdRooms(ctx, tx, h, rooms, ask.by, now, letGo); err != nil {
			return err
		}
	}
	return nil
}

// roomHolders is who holds the rooms of rows: each event or series row, and
// for an instance's own row its series (read in tx).
func roomHolders(ctx context.Context, tx *sql.Tx, rows []*models.Event) ([]*models.Event, error) {
	var heads []*models.Event
	var series []uuid.UUID
	for _, e := range rows {
		if e.SeriesID == nil {
			heads = append(heads, e)
		} else if !slices.Contains(series, *e.SeriesID) {
			series = append(series, *e.SeriesID)
		}
	}
	have := idsOf(heads)
	series = slices.DeleteFunc(series, func(id uuid.UUID) bool { return slices.Contains(have, id) })
	if len(series) == 0 {
		return heads, nil
	}
	more, err := models.GetMany(ctx, tx, series)
	return append(heads, more...), err
}

// roomHold is the rooms one event or series is to hold, and had: those it
// may keep though retired (it, or the series it was split from, held them).
type roomHold struct {
	e          *models.Event
	rooms, had []uuid.UUID
}

var errRoomsForSeries = inputError("A series books its rooms for every event: change them for all events.")

// planHolds is what each of heads is to hold once wrote (the rows written)
// are, from what each holds (held) and what the write asked. The rooms asked
// go to the row the write changed: a series split off series (which keeps
// its own), else the event or series itself; a row split off a series takes
// the series' rooms unless asked; a series whose instance alone changed keeps
// its rooms, and a different ask is refused. Pure.
func planHolds(heads []*models.Event, wrote []uuid.UUID, held map[uuid.UUID][]uuid.UUID, series uuid.UUID, ask roomsAsk) ([]roomHold, error) {
	splitOff := func(e *models.Event) bool {
		return series != uuid.Nil && e.ID != series && slices.Contains(wrote, e.ID)
	}
	split := slices.ContainsFunc(heads, splitOff)
	out := make([]roomHold, 0, len(heads))
	for _, e := range heads {
		h := roomHold{e: e, rooms: held[e.ID], had: held[e.ID]}
		switch {
		case !slices.Contains(wrote, e.ID):
			if ask.set && !sameRooms(ask.rooms, h.rooms) {
				return nil, errRoomsForSeries
			}
		case splitOff(e):
			h.had = unionRooms(slices.Clone(h.had), held[series])
			h.rooms = h.had
			if ask.set {
				h.rooms = ask.rooms
			}
		case ask.set && !split:
			h.rooms = ask.rooms
		}
		if len(h.rooms) > 0 || len(h.had) > 0 {
			out = append(out, h)
		}
	}
	return out, nil
}

// holdRooms writes h's bookings: a retired room must be one it had, and no
// occurrence of it (its overrides as written) may meet another booking of the
// room; the booking spans every occurrence. rooms are h's rooms, locked.
//
// letGo (a whole event's restore): a room booked meanwhile is given up, not
// refused.
func holdRooms(ctx context.Context, tx *sql.Tx, h roomHold, rooms map[uuid.UUID]models.Room, by uuid.UUID, now time.Time, letGo bool) error {
	e := h.e
	for _, id := range h.rooms {
		if r := rooms[id]; r.DeletedAt != nil && !slices.Contains(h.had, id) {
			return inputError(r.Name + " is retired and takes no new bookings.")
		}
	}
	start, end := bookingSpan(e)
	if e.DeletedAt == nil && e.Status != statusCancelled && len(h.rooms) > 0 {
		read := func(w models.Window) (seriesOverrides, error) {
			return overridesRead(ctx, []*models.Event{e}, w, func(ctx context.Context, ids []uuid.UUID, w models.Window) ([]*models.Event, error) {
				return models.OverridesOf(ctx, tx, ids, w)
			})
		}
		w := checkWindow(e, now)
		ovs, err := read(w)
		if err != nil {
			return err
		}
		checks, err := roomSpansIn(ctx, tx, h.rooms, reachOverrides(w, ovs[e.ID]), e.ID, end == nil)
		if err != nil {
			return err
		}
		if len(checks) > 1 {
			// A check past the first sees the writer's instances cancelled
			// or moved there too.
			if ovs, err = read(checksSpan(checks)); err != nil {
				return err
			}
		}
		mine := make([][]instance, len(checks))
		for i, c := range checks {
			if mine[i], err = checkedInstances(ctx, e, ovs, c.w); err != nil {
				return err
			}
		}
		zone, zerr := loadZone(e.TZ)
		if zerr != nil {
			zone = time.UTC
		}
		kept := h.rooms[:0:0]
		for _, id := range h.rooms {
			taken := clashIn(checks, mine, id)
			switch {
			case taken == nil:
				kept = append(kept, id)
			case !letGo:
				return &RoomTakenError{RoomID: id.String(), Room: rooms[id].Name, At: taken.start, Date: taken.start.In(zone).Format(dateLayout), other: taken.event.ID}
			}
		}
		h.rooms = kept
		start, end = widenSpan(start, end, slices.Concat(mine...))
	}
	if by == uuid.Nil {
		by = e.CreatedBy
	}
	return models.HoldRooms(pgexec.WithTx(ctx, tx), e.ID, h.rooms, start, end, by, now)
}

// widenSpan stretches a booking's span over every occurrence (an instance
// moved before its series' first or past its last). Pure.
func widenSpan(start time.Time, end *time.Time, insts []instance) (time.Time, *time.Time) {
	for _, in := range insts {
		s, e := spanOf(in.Event)
		if s.Before(start) {
			start = s
		}
		if end != nil && e.After(*end) {
			end = &e
		}
	}
	return start, end
}

// sameRooms is whether a and b are the same rooms, in any order. Pure.
func sameRooms(a, b []uuid.UUID) bool {
	return len(a) == len(b) && !slices.ContainsFunc(a, func(id uuid.UUID) bool { return !slices.Contains(b, id) })
}

// unionRooms is a with each of b it lacks. Pure.
func unionRooms(a, b []uuid.UUID) []uuid.UUID {
	for _, id := range b {
		if !slices.Contains(a, id) {
			a = append(a, id)
		}
	}
	return a
}

// firstClash is the first of theirs an occurrence of mine meets, from where
// they meet (its start: the later of the two starts). Pure.
func firstClash(mine []instance, theirs []roomSpan) *roomSpan {
	for _, in := range mine {
		s, e := spanOf(in.Event)
		for i := range theirs {
			if theirs[i].start.Before(e) && s.Before(theirs[i].end) {
				clash := theirs[i]
				if s.After(clash.start) {
					clash.start = s
				}
				return &clash
			}
		}
	}
	return nil
}

// clashIn is the first booking of room that the writer's occurrences in a
// check meet (mine[i] in checks[i]), the checks in order. Pure.
func clashIn(checks []roomCheck, mine [][]instance, room uuid.UUID) *roomSpan {
	for i, c := range checks {
		if taken := firstClash(mine[i], c.spans[room]); taken != nil {
			return taken
		}
	}
	return nil
}

// roomRefusalFor names the other event of a refusal only to who may see it.
func (s *service) roomRefusalFor(ctx context.Context, who *model.UserInfo, err error) error {
	var taken *RoomTakenError
	if !errors.As(err, &taken) {
		return err
	}
	d, derr := s.decideMany(ctx, subjectOf(who), []string{taken.other.String()})
	if dec, ok := d[taken.other.String()]; derr == nil && ok && mayViewEvent(dec) {
		if rows, gerr := models.GetMany(ctx, models.Pool(), []uuid.UUID{taken.other}); gerr == nil && len(rows) == 1 {
			taken.EventID, taken.Title = rows[0].ID.String(), rows[0].Title
		}
	}
	return taken
}

// roomsOf is the rooms each event holds, for its view; when they can't be
// read the views go without (logged).
func (s *service) roomsOf(ctx context.Context, events []uuid.UUID) map[uuid.UUID][]Room {
	held, err := s.store.heldRooms(ctx, events)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar roomsOf err: %+v", err)
		return nil
	}
	out := make(map[uuid.UUID][]Room, len(held))
	for ev, rs := range held {
		for _, r := range rs {
			out[ev] = append(out[ev], meetingRoom(r))
		}
	}
	return out
}

// roomsHeld is the rooms holder holds now, before a restore (none when they
// can't be read: roomsOf logs it).
func (s *service) roomsHeld(ctx context.Context, holder uuid.UUID) []Room {
	return s.roomsOf(ctx, []uuid.UUID{holder})[holder]
}

// roomsLetGo puts on a restore's answer the rooms of had that holder no
// longer holds: given up as another event booked them meanwhile.
func (s *service) roomsLetGo(ctx context.Context, holder uuid.UUID, had []Room, v *Event) *Event {
	if len(had) == 0 || v == nil {
		return v
	}
	now := s.roomsHeld(ctx, holder)
	for _, r := range had {
		if !slices.ContainsFunc(now, func(n Room) bool { return n.ID == r.ID }) {
			v.RoomsLetGo = append(v.RoomsLetGo, r)
		}
	}
	return v
}

// FreeRooms is the live rooms of at least capacity free all of [from, to).
func FreeRooms(ctx context.Context, who *model.UserInfo, from, to time.Time, capacity int) ([]Room, error) {
	if err := stored.membersOnlyRooms(ctx, personOf(who)); err != nil {
		return nil, err
	}
	if !to.After(from) || to.Sub(from) > maxFindTimeRange {
		return nil, inputError("Ask for free rooms over up to 14 days.")
	}
	live, err := models.LiveRooms(ctx, models.Pool())
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for _, r := range live {
		ids = append(ids, r.ID)
	}
	spans, err := roomSpansOver(ctx, ids, spanWindow(from, to))
	if err != nil {
		return nil, err
	}
	out := []Room{}
	for _, r := range live {
		taken := slices.ContainsFunc(spans[r.ID], func(sp roomSpan) bool { return sp.start.Before(to) && from.Before(sp.end) })
		if r.Capacity >= capacity && !taken {
			out = append(out, meetingRoom(r))
		}
	}
	return out, nil
}

// RoomCalendar is a room and its bookings in [from, to): busy alone where who
// may not see the event (bookingView).
func RoomCalendar(ctx context.Context, who *model.UserInfo, id string, from, to time.Time) (*Room, []RoomBooking, error) {
	s := stored
	if err := s.membersOnlyRooms(ctx, personOf(who)); err != nil {
		return nil, nil, err
	}
	if !to.After(from) || to.Sub(from) > 62*24*time.Hour {
		return nil, nil, inputError("Ask for a room's bookings over up to 62 days.")
	}
	r, err := liveRoom(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	spans, err := roomSpansOver(ctx, []uuid.UUID{r.ID}, spanWindow(from, to))
	if err != nil {
		return nil, nil, err
	}
	var evIDs []string
	var evs []uuid.UUID
	for _, sp := range spans[r.ID] {
		evIDs, evs = append(evIDs, sp.event.ID.String()), append(evs, sp.event.ID)
	}
	seen, err := s.decideMany(ctx, subjectOf(who), evIDs)
	if err != nil {
		return nil, nil, err
	}
	held := s.roomsOf(ctx, evs)
	out := []RoomBooking{}
	for _, sp := range spans[r.ID] {
		if sp.start.Before(to) && from.Before(sp.end) {
			d, ok := seen[sp.event.ID.String()]
			out = append(out, bookingView(sp, held[sp.event.ID], ok && mayViewEvent(d)))
		}
	}
	room := meetingRoom(r)
	return &room, out, nil
}

// bookingView is a booking as a reader sees it: the event and its rooms when
// they may see it, else busy alone (no title, no room names). Pure.
func bookingView(sp roomSpan, rooms []Room, mayView bool) RoomBooking {
	if !mayView {
		return RoomBooking{Start: sp.start, End: sp.end, Busy: true}
	}
	return RoomBooking{Start: sp.start, End: sp.end, EventID: sp.event.ID.String(), Title: sp.title, Rooms: rooms}
}

// RoomsBusy is when rooms are booked in [from, to), for Find a time asked by
// askerUUID (a member); a room not live is ErrNoRoom.
func RoomsBusy(ctx context.Context, askerUUID string, ids []string, from, to time.Time) ([]Interval, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	asker, _ := uuid.Parse(askerUUID)
	if err := stored.membersOnlyRooms(ctx, asker); err != nil {
		return nil, err
	}
	rooms, err := roomIDs(ids)
	if err != nil {
		return nil, err
	}
	live, err := models.RoomsByID(ctx, models.Pool(), rooms)
	if err != nil {
		return nil, err
	}
	if len(live) != len(rooms) || slices.ContainsFunc(live, func(r models.Room) bool { return r.DeletedAt != nil }) {
		return nil, ErrNoRoom
	}
	spans, err := roomSpansOver(ctx, rooms, spanWindow(from, to))
	if err != nil {
		return nil, err
	}
	var out []Interval
	for _, ss := range spans {
		for _, sp := range ss {
			out = append(out, Interval{Start: sp.start, End: sp.end})
		}
	}
	return out, nil
}

// mayViewEvent is a decision that shows the event itself, not busy alone.
// Pure.
func mayViewEvent(d accessBusiness.Decision) bool {
	return d.Level != accessBusiness.LevelNone && !busyOnly(d)
}

// askRooms is a write naming rooms: a member's only (guests and bots get
// 403), carried to the write's transaction (holdRoomsIn).
func (s *service) askRooms(ctx context.Context, who *model.UserInfo, ids []string) (context.Context, error) {
	if err := s.membersOnlyRooms(ctx, personOf(who)); err != nil {
		if len(ids) == 0 && errors.Is(err, errRoomsForMembers) {
			// Naming no room asks nothing of a guest or a bot.
			return ctx, nil
		}
		return ctx, err
	}
	rooms, err := roomIDs(ids)
	if err != nil {
		return ctx, err
	}
	return withRooms(ctx, rooms, personOf(who)), nil
}

// NameRoomClash names a refusal's other event only to who may see it.
func NameRoomClash(ctx context.Context, who *model.UserInfo, err error) error {
	return stored.roomRefusalFor(ctx, who, err)
}
