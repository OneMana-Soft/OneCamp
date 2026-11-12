package business

// Presenter notes (miro-28 2a): a frame's notes, which its presenter sees
// while presenting the board's frames as slides and the audience never does.
// They live in Postgres (models/postgres/BoardNotes), never in the board's
// document, since a share-link guest receives the whole document and every
// export carries it; this file is the store's one reader (notesGuard_test.go),
// so no export, AI context, search entry, MCP tool or audit text has them.
//
// Who: reading is CanPresent's (who may present the board: its edit right),
// writing CanEdit's; a share-link guest, a call's guest and the public never
// pass the /board routes' member gate, and no API token does either. Every
// delete is soft, but a board purged past the trash takes its notes for good
// (forgetBoardsNotes, from retention.go's files sink).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/akashc777/OneCamp/business/Board/scene"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	notesModel "github.com/akashc777/OneCamp/models/postgres/BoardNotes"
	pollModel "github.com/akashc777/OneCamp/models/postgres/Poll"
	"github.com/google/uuid"
)

// NoteMaxChars is how long a frame's note may be, in characters.
const NoteMaxChars = 10_000

// frameIDMax is how long a frame's id may be (Excalidraw's are ~21).
const frameIDMax = 128

// noteStoreLag is how long a note is shown on a frame the board's stored
// document doesn't hold: the collaboration service stores the document on a
// delay, so a frame drawn just now may not be in it yet. Past it, such a
// frame is gone (the service prunes a deleted element after ten minutes,
// storeBoard.js TOMBSTONE_PRUNE_AGE_MS) and its note is hidden, never
// deleted: a restored snapshot or an undo brings the frame back.
const noteStoreLag = 10 * time.Minute

var (
	ErrNoteTooLong     = fmt.Errorf("a frame's note is at most %d characters", NoteMaxChars)
	ErrBadFrame        = errors.New("a note names its frame, and is plain text")
	ErrNotThePresenter = errors.New("only who may present the board reads its presenter notes")
	ErrCannotEditNotes = errors.New("only who may edit the board writes its presenter notes")
)

// StaleNoteError is a write on a version that is no longer the note's:
// someone else wrote first. Current is the note as it now is, nil for none.
type StaleNoteError struct{ Current *FrameNote }

func (e *StaleNoteError) Error() string { return "someone else changed these notes" }

// FrameNote is a frame's note as its presenter reads it.
type FrameNote struct {
	FrameID   string    `json:"frame_id"`
	Body      string    `json:"body"`
	Version   int       `json:"version"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

func frameNoteOf(n notesModel.Note) FrameNote {
	return FrameNote{FrameID: n.FrameID, Body: n.Body, Version: n.Version, UpdatedBy: n.UpdatedBy.String(), UpdatedAt: n.UpdatedAt}
}

// FrameNotes is a board's presenter notes, for the frames on it, read by the
// person (userUID, userUUID): ErrBoardNotFound for one who can't read the
// board, ErrNotThePresenter for one who may read it but not present it. A
// read only filters (notesOnBoard): it never writes.
func FrameNotes(ctx context.Context, boardUUID, userUID, userUUID string) ([]FrameNote, error) {
	id, err := mayOnNotes(ctx, boardUUID, userUID, userUUID, CanPresent, ErrNotThePresenter)
	if err != nil {
		return nil, err
	}
	rows, err := notesModel.List(ctx, id)
	if err != nil || len(rows) == 0 {
		return []FrameNote{}, err
	}
	frames, readable := boardFrames(ctx, boardUUID)
	return notesOnBoard(rows, frames, readable, time.Now()), nil
}

// SaveFrameNote writes a frame's note as the person, on the version they
// read (0 for none); an empty body deletes the note (soft). It answers the
// note as written, nil once deleted. A stale version is a *StaleNoteError
// carrying the note as it now is; ErrBoardNotFound is for one who can't read
// the board, ErrCannotEditNotes for one who may not edit it.
func SaveFrameNote(ctx context.Context, boardUUID, frameID, body string, version int, userUID, userUUID string) (*FrameNote, error) {
	if err := checkNote(frameID, body); err != nil {
		return nil, err
	}
	if version < 0 {
		return nil, ErrBadFrame
	}
	id, err := mayOnNotes(ctx, boardUUID, userUID, userUUID, CanEdit, ErrCannotEditNotes)
	if err != nil {
		return nil, err
	}
	by, err := uuid.Parse(userUUID)
	if err != nil {
		return nil, ErrCannotEditNotes
	}
	n, saved, err := notesModel.Save(ctx, id, frameID, body, version, by)
	if err != nil {
		return nil, err
	}
	var note *FrameNote
	if n.Version > 0 {
		fn := frameNoteOf(n)
		note = &fn
	}
	if !saved {
		return nil, &StaleNoteError{Current: note}
	}
	return note, nil
}

// mayOnNotes reads the board for the person and asks rule of it: the board's
// id, or ErrBoardNotFound for one who can't read it, refused for one who may
// read it but not by rule.
func mayOnNotes(ctx context.Context, boardUUID, userUID, userUUID string, rule func(*dgraphStruct.DgraphBoard, string) bool, refused error) (uuid.UUID, error) {
	board, err := ReadableBoard(ctx, boardUUID, userUID, userUUID)
	if err != nil {
		return uuid.Nil, err
	}
	if !rule(board, userUUID) {
		return uuid.Nil, refused
	}
	return uuid.Parse(board.Uuid)
}

// boardFrames is the board's frames as its stored document holds them
// (scene.Frames), and whether that document could be read: a board never
// drawn on has none; one whose document couldn't be fetched or decoded is
// unreadable.
func boardFrames(ctx context.Context, boardUUID string) (map[string]bool, bool) {
	board, err := GetSystemBoardByUUID(ctx, boardUUID)
	if err != nil || board == nil || (board.State == "" && board.StateKey != "") {
		return nil, false
	}
	roots, err := scene.StateRoots(board.State)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/FrameNotes: board %s's document isn't readable: %+v", boardUUID, err)
		return nil, false
	}
	return scene.Frames(roots["elements"]), true
}

// ErrDocumentUnreadable is a board whose stored document couldn't be read.
var ErrDocumentUnreadable = errors.New("the board's document couldn't be read")

// FrameOnBoard is whether a frame is live on the board's stored document
// (scene.Frames): a poll on a slide is on one of the board's own frames
// (business/Poll board.go). A frame drawn within the store's delay isn't in
// it yet; ErrDocumentUnreadable for a document that couldn't be read.
func FrameOnBoard(ctx context.Context, boardUUID, frameID string) (bool, error) {
	frames, readable := boardFrames(ctx, boardUUID)
	if !readable {
		return false, ErrDocumentUnreadable
	}
	return frames[frameID], nil
}

// notesOnBoard is which of a board's notes show, for its frames (scene.Frames)
// at now: a note on a frame on the canvas; on one the document doesn't hold,
// a note changed within noteStoreLag (the frame may be newer than the stored
// document). Any other note is hidden and its row stays live: a frame
// deleted on the canvas comes back on an undo, and one gone from the
// document past the lag on a restored snapshot. When the document can't be
// read, every note shows. Pure.
func notesOnBoard(rows []notesModel.Note, frames map[string]bool, readable bool, now time.Time) []FrameNote {
	kept := make([]FrameNote, 0, len(rows))
	for _, n := range rows {
		live, held := frames[n.FrameID]
		if !readable || live || !held && now.Sub(n.UpdatedAt) < noteStoreLag {
			kept = append(kept, frameNoteOf(n))
		}
	}
	return kept
}

// forgetBoardsNotes removes for good every presenter note, live or deleted,
// of boards purged past the trash (retention.go's files sink, as their
// snapshots go): a purged board takes its own rows. An id that isn't a
// board's uuid has none.
func forgetBoardsNotes(ctx context.Context, boardUUIDs []string) error {
	_, err := notesModel.DeleteBoards(ctx, boardUUIDsOnly(boardUUIDs))
	return err
}

// forgetBoardsPolls removes for good the polls on a slide of boards purged
// past the trash, and their votes (retention.go's files sink, beside their
// notes): a purged board takes its own rows.
func forgetBoardsPolls(ctx context.Context, boardUUIDs []string) error {
	_, err := pollModel.DeleteBoards(ctx, boardUUIDsOnly(boardUUIDs))
	return err
}

// boardUUIDsOnly is the ids that are uuids, as uuids are written: a uuid[]
// cast would fail the whole delete on any other, which names no board.
func boardUUIDsOnly(boardUUIDs []string) []string {
	ids := make([]string, 0, len(boardUUIDs))
	for _, s := range boardUUIDs {
		if id, err := uuid.Parse(s); err == nil {
			ids = append(ids, id.String())
		}
	}
	return ids
}

// checkNote is whether a note may be written: it names a frame (at most
// frameIDMax) and is at most NoteMaxChars characters of text (no NUL, which
// Postgres text can't hold); empty is its delete.
func checkNote(frameID, body string) error {
	if frameID == "" || len(frameID) > frameIDMax || strings.ContainsRune(frameID+body, 0) {
		return ErrBadFrame
	}
	if utf8.RuneCountInString(body) > NoteMaxChars {
		return ErrNoteTooLong
	}
	return nil
}
