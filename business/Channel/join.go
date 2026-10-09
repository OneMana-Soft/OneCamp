package business

// Who may add themselves to a channel.

import (
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

var (
	// ErrJoinPrivate: a private channel is joined only by being added by
	// someone in it.
	ErrJoinPrivate = errors.New("You can't join a private channel yourself: ask someone in it to add you.")
	// ErrJoinArchived: an archived channel takes no new members.
	ErrJoinArchived = errors.New("This channel is archived.")
	// ErrJoinMissing: there's no such channel.
	ErrJoinMissing = errors.New("There's no such channel.")
)

// CanJoin says whether someone may add themselves to channel: only to a
// public channel that isn't archived. A private channel's guard used to sit
// behind an error check that could never be true, so anyone could join any
// private channel by its id and read its history.
func CanJoin(channel *dgraphStruct.DgraphChannel) error {
	switch {
	case channel == nil:
		return ErrJoinMissing
	case channel.IsPrivate == nil || *channel.IsPrivate:
		// Unknown privacy is refused: a channel's privacy that didn't load
		// must not read as public.
		return ErrJoinPrivate
	case helpers.IsSoftDeleted(channel.DeletedAt):
		return ErrJoinArchived
	}
	return nil
}
