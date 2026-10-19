package domain

// Hiding a DM or group (D-S9): it leaves its person's lists until a message
// newer than when they hid it comes, and it is theirs to show again. Leaving a
// group isn't offered; hiding it is.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/Conversation"
	"github.com/google/uuid"
)

// hiddenOf is a person's hidden conversations by ref, a seam for tests.
var (
	hiddenOf  = models.HiddenOf
	setHidden = models.SetHidden
)

// Hide hides kind ref for person (or shows it again), who is one of its people.
func Hide(ctx context.Context, kind, ref, person string, hidden bool) error {
	user, err := uuid.Parse(person)
	if err != nil {
		return err
	}
	id, err := conversation(ctx, kind, ref, "")
	if err != nil {
		return err
	}
	// The index lags the graph only by an addition it missed: the person,
	// who the graph says is in it, gets their row now.
	if m, err := memberOf(ctx, id, user); err != nil {
		return err
	} else if m == nil || !m.Live() {
		if err := joinRows(ctx, id, []uuid.UUID{user}, RoleMember, time.Now()); err != nil {
			return err
		}
	}
	var at *time.Time
	if hidden {
		now := time.Now()
		at = &now
	}
	return setHidden(ctx, id, user, at)
}

// WithoutHidden is dms without the ones person hid that have had no message
// since (each DM's newest message is its first chat). A failed read of what
// they hid shows everything.
func WithoutHidden(ctx context.Context, person string, dms []*dgraphStruct.DgraphDm) []*dgraphStruct.DgraphDm {
	user, err := uuid.Parse(person)
	if err != nil || len(dms) == 0 {
		return dms
	}
	hidden, err := hiddenOf(ctx, user)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/Conversation reading what %s hid, showing everything: %+v", person, err)
		return dms
	}
	if len(hidden) == 0 {
		return dms
	}
	out := make([]*dgraphStruct.DgraphDm, 0, len(dms))
	for _, dm := range dms {
		if dm != nil && stillHidden(dm, hidden) {
			continue
		}
		out = append(out, dm)
	}
	return out
}

// stillHidden is whether dm was hidden after its newest message. Pure.
func stillHidden(dm *dgraphStruct.DgraphDm, hidden map[string]time.Time) bool {
	at, ok := hidden[dm.GroupingId]
	if !ok {
		return false
	}
	for _, c := range dm.Chats {
		if c != nil && c.CreatedAt != nil && c.CreatedAt.After(at) {
			return false
		}
	}
	return true
}

// ContinuedIn records that a group continues in a channel.
func ContinuedIn(ctx context.Context, kind, ref string, channel uuid.UUID) error {
	id, err := conversation(ctx, kind, ref, "")
	if err != nil {
		return err
	}
	ch, err := conversation(ctx, KindChannel, channel.String(), "")
	if err != nil {
		return err
	}
	return models.SetContinuedIn(ctx, id, ch)
}
