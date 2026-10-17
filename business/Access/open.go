package business

// Open: deciding on a resource and refusing in one step, so every handler
// answers a page it may not show alike (the Wave A audit's generic function
// 1). A handler that resolves and then words its own refusal tells a stranger
// whether a page exists the moment one of them slips; this one never does.
// OpenDeleted is the same for what is done to a deleted resource itself.

import (
	"context"
	"errors"
	"fmt"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
)

var (
	// ErrGone is a resource in the trash or archived, to someone it was
	// there for before (410). It is business/Doc's ErrDocDeleted, so every
	// answer that already maps that one maps this.
	ErrGone = docBusiness.ErrDocDeleted
	// ErrLocked is something a hard lock alone keeps the person from doing:
	// without the lock their level would reach it.
	ErrLocked = errors.New("access: a hard lock keeps you from changing that")
	// ErrNotDeleted is a live resource someone may open, asked to be brought
	// back or deleted for good (409). It is business/Doc's ErrNotInTrash, so
	// the trash's answers map it already.
	ErrNotDeleted = docBusiness.ErrNotInTrash
	// ErrUndecided is a decision that couldn't be made: what it reads didn't
	// answer. It is never a no and never a yes: a handler answers it as a read
	// that failed (503, helpers.WriteReadFailure), and the cause stays
	// wrapped in it for the log.
	ErrUndecided = errors.New("access: the decision couldn't be made")
)

// undecided is a failure to decide as every decision answers it: a resource
// that isn't there stays ErrNotFound and an unknown kind stays itself, and
// anything else is ErrUndecided, wrapping its cause (once).
func undecided(err error) error {
	if err == nil || errors.Is(err, ErrUndecided) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrUnknownKind) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrUndecided, err)
}

// Open is who's decision on res, refused unless it reaches need:
//   - a deleted resource is ErrGone to someone who could open it before it
//     was deleted, and ErrNotFound to anyone else;
//   - one they can't open at all is ErrNotFound, which tells them nothing;
//   - a level below need is ErrForbidden, or ErrLocked when only a hard lock
//     holds them below it;
//   - a decision that couldn't be made is ErrUndecided, never a refusal.
//
// The decision comes back with any refusal, for a caller that words it.
func Open(ctx context.Context, who Subject, res Resource, need Level) (Decision, error) {
	d, err := Rules.Resolve(ctx, who, res)
	if err != nil {
		return d, undecided(err)
	}
	return d, refusal(d, need)
}

// OpenDeleted is Open for what is done to a deleted resource itself: bringing
// it back from the trash or the archive, or deleting it for good. It decides
// by Was, the level the person had before the delete, and before anything
// else is said:
//   - a resource never there for them is ErrNotFound, live, deleted or not
//     there at all, which tells them nothing;
//   - a live one is ErrNotDeleted;
//   - a Was below need is ErrForbidden;
//   - a decision that couldn't be made is ErrUndecided, as for Open.
//
// A workspace admin acting themselves (Subject.ActingAdmin) may do either to
// any resource there is. The decision comes back with any refusal.
func OpenDeleted(ctx context.Context, who Subject, res Resource, need Level) (Decision, error) {
	d, err := Rules.Resolve(ctx, who, res)
	if err != nil {
		return d, undecided(err)
	}
	return d, deletedRefusal(d, need, who.ActingAdmin())
}

// Refused says whether err is Open or OpenDeleted refusing the person
// (ErrGone, ErrNotFound, ErrForbidden, ErrLocked or ErrNotDeleted): the
// answer is no. Anything else is a failure to decide (ErrUndecided), which a
// caller may never take for a no.
func Refused(err error) bool {
	return errors.Is(err, ErrGone) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) ||
		errors.Is(err, ErrLocked) || errors.Is(err, ErrNotDeleted)
}

// refusal is Open's rule, for a decision made already. Pure.
func refusal(d Decision, need Level) error {
	switch {
	case d.Deleted && d.Was >= LevelView:
		return ErrGone
	case d.Deleted, d.Level == LevelNone:
		return ErrNotFound
	case d.Level >= need:
		return nil
	case d.Locked && d.Unlocked >= need:
		return ErrLocked
	}
	return ErrForbidden
}

// deletedRefusal is OpenDeleted's rule, for a decision made already. A live
// resource's Was is its Level, so one the person can't open is refused as
// one never there for them. Pure.
func deletedRefusal(d Decision, need Level, admin bool) error {
	switch {
	case d.Was < LevelView && !admin:
		return ErrNotFound
	case !d.Deleted:
		return ErrNotDeleted
	case admin, d.Was >= need:
		return nil
	}
	return ErrForbidden
}
