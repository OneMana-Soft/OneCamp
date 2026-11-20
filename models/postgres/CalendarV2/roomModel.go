package models

// Rooms (migration 601): the workspace's meeting rooms and the events that
// hold them. A room is retired (deleted_at), never removed; a booking given
// up is kept (deleted_at).

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Room is one calendar_rooms row.
type Room struct {
	ID        uuid.UUID
	Name      string
	Building  string
	Floor     string
	Capacity  int
	Features  []string
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// RoomBooking is a live booking and the event that holds it.
type RoomBooking struct {
	RoomID uuid.UUID
	Event  *Event
}

// ErrNoRoom is a room that isn't there.
var ErrNoRoom = errors.New("calendar: no such room")

const roomColumns = `id, name, building, floor, capacity, features, updated_at, deleted_at`

func scanRoom(s scanner) (Room, error) {
	var r Room
	var del sql.NullTime
	err := s.Scan(&r.ID, &r.Name, &r.Building, &r.Floor, &r.Capacity, pq.Array(&r.Features), &r.UpdatedAt, &del)
	r.DeletedAt = utcOrNil(del)
	return r, err
}

// AddRoom writes a new room; by uuid.Nil (the workspace's own) leaves its
// created_by and updated_by NULL.
func AddRoom(ctx context.Context, r Room, by uuid.UUID, at time.Time) error {
	_, err := pgexec.Exec(ctx, `INSERT INTO calendar_rooms (id, name, building, floor, capacity, features, created_by, created_at, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $7, $8)`, r.ID, r.Name, r.Building, r.Floor, r.Capacity, pq.Array(r.Features), uuid.NullUUID{UUID: by, Valid: by != uuid.Nil}, at)
	return err
}

// SetRoom writes a live room's fields; ErrNoRoom when there is none.
func SetRoom(ctx context.Context, r Room, by uuid.UUID, at time.Time) error {
	n, err := pgexec.Exec(ctx, `UPDATE calendar_rooms SET name = $2, building = $3, floor = $4, capacity = $5, features = $6,
		updated_by = $7, updated_at = $8 WHERE id = $1 AND deleted_at IS NULL`, r.ID, r.Name, r.Building, r.Floor, r.Capacity, pq.Array(r.Features), by, at)
	return oneRow(n, err)
}

// RetireRoom retires a live room; ErrNoRoom when there is none.
func RetireRoom(ctx context.Context, id, by uuid.UUID, at time.Time) error {
	n, err := pgexec.Exec(ctx, `UPDATE calendar_rooms SET deleted_at = $3, updated_by = $2, updated_at = $3 WHERE id = $1 AND deleted_at IS NULL`, id, by, at)
	return oneRow(n, err)
}

func oneRow(n int64, err error) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNoRoom
	}
	return nil
}

// RoomsByID is the rooms of ids, retired ones too.
func RoomsByID(ctx context.Context, db DB, ids []uuid.UUID) ([]Room, error) {
	return queryRows(ctx, db, "RoomsByID", `SELECT `+roomColumns+` FROM calendar_rooms WHERE id = ANY($1::uuid[]) ORDER BY lower(name)`,
		func(r *sql.Rows) (Room, error) { return scanRoom(r) }, uuidArray(ids))
}

// LiveRooms is every live room, by name.
func LiveRooms(ctx context.Context, db DB) ([]Room, error) {
	return queryRows(ctx, db, "LiveRooms", `SELECT `+roomColumns+` FROM calendar_rooms WHERE deleted_at IS NULL ORDER BY lower(name) LIMIT 2000`,
		func(r *sql.Rows) (Room, error) { return scanRoom(r) })
}

// LockRooms takes the rooms' rows for the rest of tx, in one order so two
// writers never wait on each other crosswise; their state as locked.
func LockRooms(ctx context.Context, tx DB, ids []uuid.UUID) ([]Room, error) {
	return queryRows(ctx, tx, "LockRooms", `SELECT `+roomColumns+` FROM calendar_rooms WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE`,
		func(r *sql.Rows) (Room, error) { return scanRoom(r) }, uuidArray(ids))
}

// RoomsHeldBy is the rooms each of events holds now (live bookings).
func RoomsHeldBy(ctx context.Context, db DB, events []uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
	type pair struct{ event, room uuid.UUID }
	rows, err := queryRows(ctx, db, "RoomsHeldBy", `SELECT event_id, room_id FROM calendar_room_bookings
		WHERE event_id = ANY($1::uuid[]) AND deleted_at IS NULL`, func(r *sql.Rows) (pair, error) {
		var p pair
		return p, r.Scan(&p.event, &p.room)
	}, uuidArray(events))
	out := map[uuid.UUID][]uuid.UUID{}
	for _, p := range rows {
		out[p.event] = append(out[p.event], p.room)
	}
	return out, err
}

// BookingsOn is the live bookings on rooms whose span meets [from, to), of
// live events, with each event; never those of skip; at most limit.
func BookingsOn(ctx context.Context, db DB, rooms []uuid.UUID, from, to time.Time, skip uuid.UUID, limit int) ([]RoomBooking, error) {
	return queryRows(ctx, db, "BookingsOn", `SELECT b.room_id, `+eventColumns+` FROM calendar_room_bookings b JOIN events e ON e.id = b.event_id
		WHERE b.room_id = ANY($1::uuid[]) AND b.deleted_at IS NULL AND e.deleted_at IS NULL AND e.status <> 'cancelled'
		AND b.starts_at < $3 AND (b.ends_at IS NULL OR b.ends_at > $2) AND b.event_id <> $4
		ORDER BY b.starts_at LIMIT $5`, func(r *sql.Rows) (RoomBooking, error) {
		var b RoomBooking
		e, err := scanEvent(prefixed{r, &b.RoomID})
		b.Event = e
		return b, err
	}, uuidArray(rooms), from, to, skip, limit)
}

// HoldRooms makes event's bookings exactly rooms, each over [start, end)
// (end nil: no end): adds those missing, gives up the rest, moves the spans.
// In ctx's transaction (pgexec.WithTx).
func HoldRooms(ctx context.Context, event uuid.UUID, rooms []uuid.UUID, start time.Time, end *time.Time, by uuid.UUID, at time.Time) error {
	if _, err := pgexec.Exec(ctx, `UPDATE calendar_room_bookings SET deleted_at = $3, deleted_by = $4
		WHERE event_id = $1 AND deleted_at IS NULL AND NOT room_id = ANY($2::uuid[])`, event, uuidArray(rooms), at, by); err != nil {
		return err
	}
	if _, err := pgexec.Exec(ctx, `UPDATE calendar_room_bookings SET starts_at = $2, ends_at = $3 WHERE event_id = $1 AND deleted_at IS NULL`,
		event, start, end); err != nil {
		return err
	}
	if len(rooms) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(rooms))
	for i := range ids {
		ids[i] = uuid.New()
	}
	_, err := pgexec.Exec(ctx, `INSERT INTO calendar_room_bookings (id, room_id, event_id, starts_at, ends_at, created_by, created_at)
		SELECT b.id, b.room, $3, $4, $5, $6, $7 FROM unnest($1::uuid[], $2::uuid[]) AS b(id, room)
		ON CONFLICT (room_id, event_id) WHERE deleted_at IS NULL DO NOTHING`, uuidArray(ids), uuidArray(rooms), event, start, end, by, at)
	return err
}

// prefixed scans one column into first, then the rest as s's caller asks.
type prefixed struct {
	s     scanner
	first any
}

func (p prefixed) Scan(dest ...any) error { return p.s.Scan(append([]any{p.first}, dest...)...) }
