// Package business (Notice) is the one-time notices: a few words the app
// shows a person once, on whichever device, until they dismiss it. Each
// notice decides whom it is for now; models/postgres/Notice keeps who has
// seen which.
package business

import (
	"context"
	"errors"

	noticeModel "github.com/akashc777/OneCamp/models/postgres/Notice"
	"github.com/google/uuid"
)

// Shown is a notice as the app shows it to one person: its key, and what it
// says to them.
type Shown struct {
	Key  string            `json:"key"`
	Data map[string]string `json:"data,omitempty"`
}

// Person is who a notice is asked about.
type Person struct {
	// UserID is their id, the same in Postgres and the graph.
	UserID uuid.UUID
	// UserUID is their graph uid.
	UserUID string
	// WorkspaceAdmin is whether they are one of the workspace's admins.
	WorkspaceAdmin bool
}

// notice is one notice: its key, and whether it is for someone now (and
// what it says to them), nil for not.
type notice struct {
	key   string
	shows func(ctx context.Context, p Person) (*Shown, error)
}

// ErrUnknown is a notice that isn't one.
var ErrUnknown = errors.New("notices: no such notice")

// The notices, in the order they show; and the store, a seam for tests.
var (
	notices  = []notice{membersEdit}
	seenOf   = noticeModel.SeenOf
	markSeen = noticeModel.MarkSeen
)

// For is the notices for the person now that they haven't dismissed, in
// order. A notice that can't decide is an error, never shown on a guess.
func For(ctx context.Context, p Person) ([]Shown, error) {
	keys := make([]string, 0, len(notices))
	for _, n := range notices {
		keys = append(keys, n.key)
	}
	seen, err := seenOf(ctx, p.UserID, keys)
	if err != nil {
		return nil, err
	}
	out := []Shown{}
	for _, n := range notices {
		if seen[n.key] {
			continue
		}
		s, err := n.shows(ctx, p)
		if err != nil {
			return nil, err
		}
		if s != nil {
			out = append(out, *s)
		}
	}
	return out, nil
}

// Dismiss records that the person has seen the notice, for good and on every
// device; dismissing it twice is once.
func Dismiss(ctx context.Context, userID uuid.UUID, key string) error {
	for _, n := range notices {
		if n.key == key {
			return markSeen(ctx, userID, key)
		}
	}
	return ErrUnknown
}
