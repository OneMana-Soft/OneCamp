package models

// A board's presenter notes (miro-28 2a, migration 588_board_frame_notes): one
// live note per frame, each write on the version its writer read; a delete is
// soft, but a purged board's go for good. Who may read and write them is
// business/Board's (notes.go), the one reader.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Note is a frame's live note.
type Note struct {
	FrameID   string
	Body      string
	Version   int
	UpdatedBy uuid.UUID
	UpdatedAt time.Time
}

const noteColumns = `frame_id, body, version, updated_by, updated_at`

func scanNote(r pgexec.Row) (n Note, err error) {
	err = r.Scan(&n.FrameID, &n.Body, &n.Version, &n.UpdatedBy, &n.UpdatedAt)
	return n, err
}

// List is a board's live notes, by frame id.
func List(ctx context.Context, board uuid.UUID) ([]Note, error) {
	return pgexec.Query(ctx, `SELECT `+noteColumns+` FROM board_frame_notes
		WHERE board_uuid = $1 AND deleted_at IS NULL ORDER BY frame_id`, scanNote, board)
}

// Save writes a frame's note as by, on the version they read: 0 for no note,
// which makes one; the note's version, which changes it; an empty body on the
// note's version deletes it (soft). It answers the note as written (the zero
// Note once deleted) and true, or, when the version read is no longer the
// note's (someone else wrote first), the note as it now is (the zero Note
// for none) and false, writing nothing.
func Save(ctx context.Context, board uuid.UUID, frame, body string, version int, by uuid.UUID) (Note, bool, error) {
	var (
		n     Note
		saved bool
		err   error
	)
	switch {
	case version == 0 && body == "":
		// Nothing to delete: right while the frame still has no note (read below).
	case version == 0:
		n, saved, err = pgexec.QueryOne(ctx, `INSERT INTO board_frame_notes
			(id, board_uuid, frame_id, body, version, created_by, updated_by)
			VALUES ($1, $2, $3, $4, 1, $5, $5)
			ON CONFLICT (board_uuid, frame_id) WHERE deleted_at IS NULL DO NOTHING
			RETURNING `+noteColumns, scanNote, uuid.New(), board, frame, body, by)
	case body == "":
		_, saved, err = pgexec.QueryOne(ctx, `UPDATE board_frame_notes
			SET deleted_at = now(), deleted_by = $4
			WHERE board_uuid = $1 AND frame_id = $2 AND deleted_at IS NULL AND version = $3
			RETURNING `+noteColumns, scanNote, board, frame, version, by)
	default:
		n, saved, err = pgexec.QueryOne(ctx, `UPDATE board_frame_notes
			SET body = $4, version = version + 1, updated_by = $5, updated_at = now()
			WHERE board_uuid = $1 AND frame_id = $2 AND deleted_at IS NULL AND version = $3
			RETURNING `+noteColumns, scanNote, board, frame, version, body, by)
	}
	if err != nil || saved {
		return n, saved, err
	}
	n, _, err = pgexec.QueryOne(ctx, `SELECT `+noteColumns+` FROM board_frame_notes
		WHERE board_uuid = $1 AND frame_id = $2 AND deleted_at IS NULL`, scanNote, board, frame)
	if version == 0 && body == "" && err == nil && n.Version == 0 {
		return n, true, nil
	}
	return n, false, err
}

// DeleteBoards removes for good every note, live or deleted, of boards
// purged past the trash (a purged board takes its own rows). Answers how many
// it removed.
func DeleteBoards(ctx context.Context, boards []string) (int64, error) {
	if len(boards) == 0 {
		return 0, nil
	}
	return pgexec.Exec(ctx, `DELETE FROM board_frame_notes WHERE board_uuid = ANY($1::uuid[])`, pq.Array(boards))
}
