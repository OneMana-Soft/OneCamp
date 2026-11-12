package business

import (
	"errors"
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	notesModel "github.com/akashc777/OneCamp/models/postgres/BoardNotes"
	"github.com/google/uuid"
)

// Presenter notes (miro-28 2a) are read by who may present the board, and by
// nobody else: the web's Present and a call's present_board both ask CanPresent,
// the board's edit right, so a member who may only view or comment reads none.
func TestOnlyWhoMayPresentReadsTheNotes(t *testing.T) {
	yes, no := true, false
	deleted := time.Now().Add(-time.Hour)
	owner := &dgraphStruct.DgraphUser{Uuid: "owner"}
	cases := []struct {
		name  string
		board *dgraphStruct.DgraphBoard
		user  string
		want  bool
	}{
		{"no board", nil, "me", false},
		{"its creator", &dgraphStruct.DgraphBoard{IsPrivate: &yes, CreatedBy: owner}, "owner", true},
		{"shared to edit", &dgraphStruct.DgraphBoard{IsPrivate: &yes, CreatedBy: owner, HasEditAccess: 1}, "me", true},
		{"shared to view", &dgraphStruct.DgraphBoard{IsPrivate: &yes, CreatedBy: owner, HasReadAccess: 1}, "me", false},
		{"shared to comment", &dgraphStruct.DgraphBoard{IsPrivate: &yes, CreatedBy: owner, HasCommentAccess: 1}, "me", false},
		{"a workspace board anyone may open", &dgraphStruct.DgraphBoard{IsPrivate: &no, CreatedBy: owner}, "me", false},
		{"deleted, its creator", &dgraphStruct.DgraphBoard{IsPrivate: &yes, CreatedBy: owner, DeletedAt: &deleted}, "owner", false},
	}
	for _, c := range cases {
		if got := CanPresent(c.board, c.user); got != c.want {
			t.Errorf("%s: CanPresent = %v, want %v", c.name, got, c.want)
		}
	}
}

// A board's notes show only for the frames on it. A frame deleted on the
// canvas keeps its note, hidden, so undoing the delete brings it back; one
// gone from the board's document past the store's delay hides its note too,
// and deletes nothing (a read never writes, and a restored document brings
// the frame back). A frame newer than the stored state (the collaboration
// service stores on a delay) keeps a fresh note shown; and a state that can't
// be read hides nothing.
func TestNotesShowOnlyForFramesOnTheBoard(t *testing.T) {
	now := time.Now()
	by := uuid.New()
	rows := []notesModel.Note{
		{FrameID: "live", Body: "Open with the numbers", Version: 2, UpdatedBy: by, UpdatedAt: now.Add(-time.Hour)},
		{FrameID: "erased", Body: "Skip this one", Version: 1, UpdatedBy: by, UpdatedAt: now.Add(-time.Hour)},
		{FrameID: "new", Body: "Just drawn", Version: 1, UpdatedBy: by, UpdatedAt: now.Add(-time.Minute)},
		{FrameID: "gone", Body: "Long pruned", Version: 3, UpdatedBy: by, UpdatedAt: now.Add(-20 * time.Minute)},
	}
	frames := map[string]bool{"live": true, "erased": false}
	ids := func(ns []FrameNote) string {
		var out []string
		for _, n := range ns {
			out = append(out, n.FrameID)
		}
		return strings.Join(out, ",")
	}

	kept := notesOnBoard(rows, frames, true, now)
	if got := ids(kept); got != "live,new" {
		t.Errorf("shown: %s, want live,new", got)
	}
	if kept[0].Body != "Open with the numbers" || kept[0].Version != 2 || kept[0].UpdatedBy != by.String() {
		t.Errorf("a note carries its body, version and writer: %+v", kept[0])
	}

	kept = notesOnBoard(rows, nil, false, now)
	if got := ids(kept); got != "live,erased,new,gone" {
		t.Errorf("an unreadable state: shown %s; want all shown", got)
	}
}

// A note is at most 10,000 characters (counted as characters, not bytes) and
// names a frame; an empty note is allowed: it is the note's soft delete.
func TestANoteIsCappedAndNamesAFrame(t *testing.T) {
	for _, c := range []struct {
		name, frame, body string
		want              error
	}{
		{"a note", "f1", "Open with the numbers", nil},
		{"empty: its delete", "f1", "", nil},
		{"10,000 characters of two bytes each", "f1", strings.Repeat("é", NoteMaxChars), nil},
		{"one character more", "f1", strings.Repeat("é", NoteMaxChars+1), ErrNoteTooLong},
		{"no frame", "", "Hi", ErrBadFrame},
		{"a frame id past 128", strings.Repeat("f", 129), "Hi", ErrBadFrame},
	} {
		if err := checkNote(c.frame, c.body); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

// A purge's uuid[] deletes (notes, polls) get only the ids that are uuids, in
// one spelling: one id that isn't would fail the whole delete and leave every
// purged board's rows.
func TestBoardUUIDsOnlyKeepsOnlyUUIDs(t *testing.T) {
	id := uuid.NewString()
	got := boardUUIDsOnly([]string{"", "not-a-uuid", strings.ToUpper(id)})
	if len(got) != 1 || got[0] != id {
		t.Errorf("boardUUIDsOnly gives %q, want only %q", got, id)
	}
}
