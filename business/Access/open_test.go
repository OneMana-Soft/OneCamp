package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
)

// Open's rule, for every handler alike: a deleted page tells only the people
// it was there for that it was deleted; a page someone can't open tells them
// nothing; a level short of what's needed is refused, and a hard lock says so
// when the lock alone holds them back.
func TestOpenRefusesAsEveryHandlerShould(t *testing.T) {
	for _, c := range []struct {
		name string
		d    Decision
		need Level
		want error
	}{
		{"a viewer opens it", Decision{Level: LevelView, Unlocked: LevelView, Was: LevelView}, LevelView, nil},
		{"an editor edits it", Decision{Level: LevelEdit, Unlocked: LevelEdit, Was: LevelEdit}, LevelEdit, nil},
		{"full access shares it", Decision{Level: LevelFull, Unlocked: LevelFull, Was: LevelFull}, LevelFull, nil},
		{"a stranger learns nothing", Decision{}, LevelView, ErrNotFound},
		{"deleted, to someone it was there for", Decision{Deleted: true, Was: LevelView}, LevelView, ErrGone},
		{"deleted, to its owner", Decision{Deleted: true, Owner: true, Was: LevelFull}, LevelEdit, ErrGone},
		{"deleted, to anyone else", Decision{Deleted: true}, LevelView, ErrNotFound},
		{"a viewer may not edit", Decision{Level: LevelView, Unlocked: LevelView, Was: LevelView}, LevelEdit, ErrForbidden},
		{"an editor may not share", Decision{Level: LevelEdit, Unlocked: LevelEdit, Was: LevelEdit}, LevelFull, ErrForbidden},
		{"a hard lock holds an editor", Decision{Level: LevelComment, Unlocked: LevelEdit, Was: LevelComment, Locked: true}, LevelEdit, ErrLocked},
		{"a hard-locked editor still comments", Decision{Level: LevelComment, Unlocked: LevelEdit, Was: LevelComment, Locked: true}, LevelComment, nil},
		{"a hard lock never made an editor full", Decision{Level: LevelComment, Unlocked: LevelEdit, Was: LevelComment, Locked: true}, LevelFull, ErrForbidden},
	} {
		if got := refusal(c.d, c.need); !errors.Is(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if !errors.Is(ErrGone, docBusiness.ErrDocDeleted) {
		t.Error("a page in the trash answers as business/Doc's deleted doc, which every handler maps already")
	}
}

// Open decides through Rules, and answers its decision with any refusal.
func TestOpenDecidesThroughRules(t *testing.T) {
	was := Rules
	defer func() { Rules = was }()
	Rules = fakeRules{levels: map[string]Level{"mine": LevelEdit}}
	ctx := context.Background()
	if d, err := Open(ctx, Subject{}, Resource{KindDoc, "mine"}, LevelEdit); err != nil || d.Level != LevelEdit {
		t.Errorf("editing what I may edit: %+v %v", d, err)
	}
	if d, err := Open(ctx, Subject{}, Resource{KindDoc, "mine"}, LevelFull); !errors.Is(err, ErrForbidden) || d.Level != LevelEdit {
		t.Errorf("sharing what I may only edit: %+v %v", d, err)
	}
	if _, err := Open(ctx, Subject{}, Resource{KindDoc, "someone else's"}, LevelView); !errors.Is(err, ErrNotFound) {
		t.Errorf("a page that isn't there for me: %v", err)
	}
}

// OpenDeleted's rule, for what is done to a deleted page itself: bringing it
// back (edit) or deleting it for good (full). A page never there for someone
// tells them nothing, live or deleted; someone it was there for hears that a
// live one isn't in the trash, or that the level they had falls short; a
// workspace admin may do either to any page there is.
func TestOpenDeletedSaysNothingBeforeTheLevel(t *testing.T) {
	view := Decision{Level: LevelView, Unlocked: LevelView, Was: LevelView}
	for _, c := range []struct {
		name  string
		d     Decision
		need  Level
		admin bool
		want  error
	}{
		{"a live page, to a stranger", Decision{}, LevelEdit, false, ErrNotFound},
		{"a page in the trash, to a stranger", Decision{Deleted: true}, LevelEdit, false, ErrNotFound},
		{"an archived page, to a stranger", Decision{Deleted: true, Archived: true}, LevelFull, false, ErrNotFound},
		{"a live page, to a reader", view, LevelEdit, false, ErrNotDeleted},
		{"a live page, to an admin", Decision{}, LevelFull, true, ErrNotDeleted},
		{"in the trash, to someone who could only read it", Decision{Deleted: true, Was: LevelView}, LevelEdit, false, ErrForbidden},
		{"in the trash, to an editor bringing it back", Decision{Deleted: true, Was: LevelEdit}, LevelEdit, false, nil},
		{"in the trash, to an editor deleting it for good", Decision{Deleted: true, Was: LevelEdit}, LevelFull, false, ErrForbidden},
		{"in the trash, to its owner", Decision{Deleted: true, Owner: true, Was: LevelFull}, LevelFull, false, nil},
		{"archived, to someone with full access", Decision{Deleted: true, Archived: true, Was: LevelFull}, LevelEdit, false, nil},
		{"in the trash, to an admin it was never there for", Decision{Deleted: true}, LevelFull, true, nil},
	} {
		if got := deletedRefusal(c.d, c.need, c.admin); !errors.Is(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if !errors.Is(ErrNotDeleted, docBusiness.ErrNotInTrash) {
		t.Error("a live page answers as business/Doc's ErrNotInTrash, which the trash's answers map already")
	}
}

// OpenDeleted decides through Rules, and only an admin acting themselves is
// one: an agent acting for an admin is refused what its asker never had.
func TestOpenDeletedDecidesThroughRules(t *testing.T) {
	was := Rules
	defer func() { Rules = was }()
	ctx, page := context.Background(), Resource{KindDoc, "page"}
	Rules = oneDecision{d: Decision{Deleted: true, Was: LevelEdit}}
	if d, err := OpenDeleted(ctx, Subject{}, page, LevelEdit); err != nil || !d.Deleted {
		t.Errorf("an editor bringing back a page in the trash: %+v %v", d, err)
	}
	if _, err := OpenDeleted(ctx, Subject{}, page, LevelFull); !errors.Is(err, ErrForbidden) {
		t.Errorf("an editor deleting it for good: %v", err)
	}
	Rules = oneDecision{d: Decision{Deleted: true}}
	if _, err := OpenDeleted(ctx, Subject{Admin: true}, page, LevelFull); err != nil {
		t.Errorf("an admin deleting it for good: %v", err)
	}
	if _, err := OpenDeleted(ctx, Subject{Admin: true, AgentUUID: "agent"}, page, LevelFull); !errors.Is(err, ErrNotFound) {
		t.Errorf("an agent acting for an admin: %v", err)
	}
	Rules = fakeRules{}
	if _, err := OpenDeleted(ctx, Subject{Admin: true}, page, LevelEdit); !errors.Is(err, ErrNotFound) {
		t.Errorf("a page that isn't there, even to an admin: %v", err)
	}
}

// A decision that couldn't be made is ErrUndecided, wrapping its cause, from
// Open and OpenDeleted alike: never a refusal, so no handler answers it as a
// no, and never "not found". A resource that isn't there stays ErrNotFound.
func TestADecisionThatCantBeMadeIsUndecided(t *testing.T) {
	was := Rules
	defer func() { Rules = was }()
	ctx, page, cause := context.Background(), Resource{KindDoc, "page"}, errors.New("graph down")
	Rules = failingRules{err: cause}
	for name, open := range map[string]func() error{
		"Open":        func() error { _, err := Open(ctx, Subject{}, page, LevelView); return err },
		"OpenDeleted": func() error { _, err := OpenDeleted(ctx, Subject{}, page, LevelEdit); return err },
	} {
		err := open()
		if !errors.Is(err, ErrUndecided) || !errors.Is(err, cause) || Refused(err) || errors.Is(err, ErrNotFound) {
			t.Errorf("%s while the graph is down: %v", name, err)
		}
	}
	Rules = failingRules{err: fmt.Errorf("reading: %w", ErrNotFound)}
	if _, err := Open(ctx, Subject{}, page, LevelView); errors.Is(err, ErrUndecided) || !Refused(err) {
		t.Errorf("a page that isn't there: %v, want a refusal", err)
	}
	if err := undecided(undecided(cause)); strings.Count(err.Error(), ErrUndecided.Error()) != 1 {
		t.Errorf("wrapped twice: %v", err)
	}
	if undecided(ErrUnknownKind) != ErrUnknownKind || undecided(nil) != nil {
		t.Error("an unknown kind, or no error, is left as it is")
	}
}

// failingRules is a resolver that can't decide.
type failingRules struct {
	Resolver
	err error
}

func (f failingRules) Resolve(context.Context, Subject, Resource) (Decision, error) {
	return Decision{}, f.err
}

// A refusal is a no; a failure to decide is not one, and never passes for it.
func TestARefusalIsNotAFailure(t *testing.T) {
	for _, err := range []error{ErrGone, ErrNotFound, ErrForbidden, ErrLocked, ErrNotDeleted, fmt.Errorf("deciding: %w", ErrForbidden)} {
		if !Refused(err) {
			t.Errorf("%v is a refusal", err)
		}
	}
	for _, err := range []error{nil, errors.New("graph down"), context.DeadlineExceeded, ErrUndecided, undecided(errors.New("graph down"))} {
		if Refused(err) {
			t.Errorf("%v isn't a refusal", err)
		}
	}
}
