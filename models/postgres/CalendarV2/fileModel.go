package models

// An event's files (migrations/575_calendar_event_files, gcal-1a head B): the
// OneCamp docs and boards an event carries, by reference. A file taken off is
// marked, never deleted, and adding it again brings its row back.

import (
	"context"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// EventFile is one OneCamp doc or board an event carries, by reference.
type EventFile struct {
	EventID uuid.UUID
	Kind    string
	Ref     uuid.UUID
	AddedBy uuid.UUID
	AddedAt time.Time
}

// ErrFilesFull is a file added to an event that carries its most already.
var ErrFilesFull = errors.New("calendar: the event carries its most files")

// FilesOf is each of these events' live files, in the order they were put on.
func FilesOf(ctx context.Context, events []uuid.UUID) (map[uuid.UUID][]EventFile, error) {
	out := map[uuid.UUID][]EventFile{}
	if len(events) == 0 {
		return out, nil
	}
	rows, err := pgexec.Query(ctx, `SELECT event_id, ref_kind, ref_uuid, added_by, added_at FROM calendar_event_files
		WHERE event_id = ANY($1::uuid[]) AND deleted_at IS NULL
		ORDER BY added_at, id`,
		func(r pgexec.Row) (EventFile, error) {
			var f EventFile
			err := r.Scan(&f.EventID, &f.Kind, &f.Ref, &f.AddedBy, &f.AddedAt)
			return f, err
		}, uuidArray(events))
	if err != nil {
		return nil, err
	}
	for _, f := range rows {
		f.AddedAt = f.AddedAt.UTC()
		out[f.EventID] = append(out[f.EventID], f)
	}
	return out, nil
}

// AddFile puts f on its event, live, while it carries fewer than most others:
// one taken off comes back (its own row, stamped by its new adder), and one
// already on it stays as it is. It says whether the event's files changed,
// and ErrFilesFull when there was no room. The count and the write hold the
// event's lock (pgexec.Capped), so two at once never pass the most.
func AddFile(ctx context.Context, f EventFile, most int) (bool, error) {
	changed := false
	others := func(ctx context.Context) (int, error) {
		return pgexec.Scalar[int](ctx, `SELECT count(*) FROM calendar_event_files
			WHERE event_id = $1 AND deleted_at IS NULL AND NOT (ref_kind = $2 AND ref_uuid = $3)`, f.EventID, f.Kind, f.Ref)
	}
	room, err := pgexec.Capped(ctx, "calendar_event_files:"+f.EventID.String(), most, others, func(ctx context.Context) error {
		n, err := pgexec.Exec(ctx, `UPDATE calendar_event_files SET deleted_at = NULL, deleted_by = NULL, added_by = $4, added_at = $5
			WHERE id = (SELECT id FROM calendar_event_files
			            WHERE event_id = $1 AND ref_kind = $2 AND ref_uuid = $3 AND deleted_at IS NOT NULL
			            ORDER BY deleted_at DESC, id LIMIT 1)
			  AND NOT EXISTS (SELECT 1 FROM calendar_event_files
			                  WHERE event_id = $1 AND ref_kind = $2 AND ref_uuid = $3 AND deleted_at IS NULL)`,
			f.EventID, f.Kind, f.Ref, f.AddedBy, f.AddedAt)
		if err == nil && n == 0 {
			n, err = pgexec.Exec(ctx, `INSERT INTO calendar_event_files (id, event_id, ref_kind, ref_uuid, added_by, added_at)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (event_id, ref_kind, ref_uuid) WHERE deleted_at IS NULL DO NOTHING`,
				uuid.New(), f.EventID, f.Kind, f.Ref, f.AddedBy, f.AddedAt)
		}
		changed = n > 0
		return err
	})
	if err != nil {
		return false, err
	}
	if !room {
		return false, ErrFilesFull
	}
	return changed, nil
}

// AddFiles puts files on a new event in one statement, in the transaction ctx
// carries (its create's, or the batch that made it from a series): checked
// and few (the caller's most, or a series' own), and none on it yet, so
// there is nothing to bring back or count. Their ids are in time order
// (version 7, monotonic), so files put on at the same moment read back in the
// order given (FilesOf orders by added_at, then id).
func AddFiles(ctx context.Context, files []EventFile) error {
	if len(files) == 0 {
		return nil
	}
	ids, events, kinds, refs, by, at := make([]uuid.UUID, len(files)), make([]uuid.UUID, len(files)), make([]string, len(files)),
		make([]uuid.UUID, len(files)), make([]uuid.UUID, len(files)), make([]string, len(files))
	for i, f := range files {
		ids[i], events[i], kinds[i], refs[i], by[i] = uuid.Must(uuid.NewV7()), f.EventID, f.Kind, f.Ref, f.AddedBy
		at[i] = f.AddedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := pgexec.Exec(ctx, `INSERT INTO calendar_event_files (id, event_id, ref_kind, ref_uuid, added_by, added_at)
		SELECT * FROM unnest($1::uuid[], $2::uuid[], $3::text[], $4::uuid[], $5::uuid[], $6::timestamptz[])
		ON CONFLICT (event_id, ref_kind, ref_uuid) WHERE deleted_at IS NULL DO NOTHING`,
		uuidArray(ids), uuidArray(events), pq.Array(kinds), uuidArray(refs), uuidArray(by), pq.Array(at))
	return err
}

// RemoveFile takes a file off its event, marked by who at at, and says
// whether it was on it.
func RemoveFile(ctx context.Context, event uuid.UUID, kind string, ref, by uuid.UUID, at time.Time) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE calendar_event_files SET deleted_at = $5, deleted_by = $4
		WHERE event_id = $1 AND ref_kind = $2 AND ref_uuid = $3 AND deleted_at IS NULL`, event, kind, ref, by, at)
	return n > 0, err
}
