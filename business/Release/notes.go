package business

// A release's notes drafted by the AI edition (linear-36). business/AI fills
// the drafter from its init; the AI-free edition has none, so there are no
// drafts there (helpers.ErrNotFound) and this package never imports AI.

import (
	"context"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
	githubTaskActivityModel "github.com/akashc777/OneCamp/models/postgres/GitHubTaskActivity"
	releaseModel "github.com/akashc777/OneCamp/models/postgres/Release"
	"github.com/google/uuid"
)

// NotesBrief is what a release's notes are drafted from: its name and the
// tasks its asker may read, each with its pull requests' titles.
type NotesBrief struct {
	Release string
	Tasks   []NotesTask
}

// NotesTask is one task of a NotesBrief.
type NotesTask struct {
	Key, Name, Status string
	PRs               []string
}

// NotesDrafter drafts notes from a brief for the asker (their user uuid), on
// their own model and allowance. An error it answers with is one written for
// them (DraftRefusal) or one the server logs.
type NotesDrafter func(ctx context.Context, asker string, b NotesBrief) (string, error)

// DraftRefusal is a draft the drafter declined, with the HTTP status and the
// words to answer with (the AI off, the asker over their limit).
type DraftRefusal struct {
	Code int
	Msg  string
}

func (e *DraftRefusal) Error() string { return e.Msg }

var notesDrafter NotesDrafter

// prTitles is a seam for tests: each task's pull requests' titles.
var prTitles = githubTaskActivityModel.PRTitles

// RegisterNotesDrafter sets the drafter: business/AI's init, on the AI
// edition only (nil takes it away). It answers the drafter it replaced, so
// a test that swaps it can put it back.
func RegisterNotesDrafter(d NotesDrafter) NotesDrafter {
	was := notesDrafter
	notesDrafter = d
	return was
}

// DraftsNotes is whether this edition drafts notes.
func DraftsNotes() bool { return notesDrafter != nil }

// DraftNotes drafts r's notes for who from what they may read of it
// (Releases.Get: a restricted, deleted or moved task never reaches the
// drafter). Saving them is Update. No drafter: helpers.ErrNotFound.
func DraftNotes(ctx context.Context, who accessBusiness.Subject, r releaseModel.Release) (string, error) {
	draft := notesDrafter
	if draft == nil {
		return "", helpers.ErrNotFound
	}
	rel, err := Releases.Get(ctx, who, r.ID)
	if err != nil {
		return "", err
	}
	ids := make([]uuid.UUID, 0, len(rel.Tasks))
	for _, t := range rel.Tasks {
		if id, err := uuid.Parse(t.UUID); err == nil {
			ids = append(ids, id)
		}
	}
	titles, err := prTitles(ctx, ids)
	if err != nil {
		return "", err
	}
	b := NotesBrief{Release: rel.Name, Tasks: make([]NotesTask, 0, len(rel.Tasks))}
	for _, t := range rel.Tasks {
		id, _ := uuid.Parse(t.UUID)
		b.Tasks = append(b.Tasks, NotesTask{Key: t.Key, Name: t.Name, Status: t.Status, PRs: titles[id]})
	}
	return draft(ctx, who.UserUUID, b)
}
