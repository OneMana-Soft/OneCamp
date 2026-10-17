package businness

// Who may read a doc, of many people at once (the A5×A6 join): the access rule
// (docAccessDecide.go) decided from the doc's side, in one read of its sharing,
// for the notices, mentions and follows that must reach exactly the people who
// can read it. Its owner, the people and teams it is shared with, its space's
// members and its general access all count, as in every check. A grant whose
// end has passed counts as gone already: the expiry sweep (business/Access
// expiry.go) takes it away within the minute, and nobody is told about a doc in
// that minute after their grant on it ended.
//
// The Access Resolver asks this for many people at once (business/Access
// ResolvePeople); business/Comment, which this package imports and so can't
// import it, is handed Readers when the server starts.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	businessComment "github.com/akashc777/OneCamp/business/Comment"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	accessModels "github.com/akashc777/OneCamp/models/postgres/Access"
	"github.com/google/uuid"
)

// errNoStore is the grants that end being unreadable for want of a database:
// who may read the doc can't be said, so nobody is told.
var errNoStore = errors.New("doc readers: the grants that end can't be read")

// Seams for tests: the doc's sharing, the grants on it that end, and the clock.
var (
	readersSharing = ReadSharing
	readersEnding  = func(ctx context.Context, docUUID string) ([]*accessModels.Expiry, error) {
		id, err := uuid.Parse(docUUID)
		if err != nil {
			return nil, ErrNotFound
		}
		if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
			return nil, errNoStore
		}
		return accessModels.ListExpiry(ctx, "doc", id)
	}
	readersNow = time.Now
	// endedOn is which of the docs hold a grant whose end has passed.
	endedOn = func(ctx context.Context, docUUIDs []string) (map[string]bool, error) {
		out := map[string]bool{}
		if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
			// No database: no grant was ever given until a date.
			return out, nil
		}
		ids := make([]uuid.UUID, 0, len(docUUIDs))
		for _, id := range docUUIDs {
			if u, err := uuid.Parse(id); err == nil {
				ids = append(ids, u)
			}
		}
		got, err := accessModels.EndedOn(ctx, "doc", ids, readersNow())
		if err != nil {
			return nil, err
		}
		for id := range got {
			out[id.String()] = true
		}
		return out, nil
	}
)

func init() {
	businessComment.RegisterDocReaders(Readers)
}

// DecidePeople is each person's decision on a doc, for people named by uuid
// and keyed as given (an empty id is left out): one read of the doc's sharing
// and one of the grants on it that end, whoever is asked about. ErrNotFound
// for no such doc; any error says nothing about anyone.
func DecidePeople(ctx context.Context, docUUID string, userUUIDs []string) (map[string]DocDecision, error) {
	out := make(map[string]DocDecision, len(userUUIDs))
	if len(userUUIDs) == 0 {
		return out, nil
	}
	s, err := readersSharing(ctx, docUUID)
	if err != nil {
		return nil, err
	}
	ends, err := readersEnding(ctx, docUUID)
	if err != nil {
		return nil, err
	}
	s = withoutEnded(s, ends, readersNow())
	for _, id := range userUUIDs {
		if id != "" {
			out[id] = s.DecisionOf(id)
		}
	}
	return out, nil
}

// Readers is, of the people named, who may read the doc now (view or more by
// DecidePeople), keyed as given; false for the rest.
func Readers(ctx context.Context, docUUID string, userUUIDs []string) (map[string]bool, error) {
	decided, err := DecidePeople(ctx, docUUID, userUUIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(decided))
	for id, d := range decided {
		out[id] = d.Level >= level.View
	}
	return out, nil
}

// withoutEnded is the sharing with every grant whose end has passed taken
// away, as the expiry sweep takes it (business/Access endGrant); s itself is
// left alone. Pure.
func withoutEnded(s *Sharing, ends []*accessModels.Expiry, now time.Time) *Sharing {
	c := SharingChange{People: map[string]level.Level{}, Teams: map[string]level.Level{}}
	ended := false
	for _, e := range ends {
		if e == nil || e.ExpiresAt.After(now) {
			continue
		}
		id := strings.ToLower(e.PrincipalUUID.String())
		if e.PrincipalKind == "team" {
			c.Teams[id] = level.None
		} else {
			c.People[id] = level.None
		}
		ended = true
	}
	if !ended {
		return s
	}
	return s.With(c)
}

// withEndedGrants is the person's decisions (keyed by lower-case doc uuid)
// with every grant whose end has passed taken away, as on the doc's side
// (DecidePeople): opening a doc, its live socket, the lists and the inbox
// decide as the notices do, in the minute before the expiry sweep runs. A doc
// with such a grant is decided again from its side, for the person by uuid;
// with no uuid to decide by, it is closed to them until the sweep has run.
// Usually no doc has one, and this is one small read.
func withEndedGrants(ctx context.Context, v Viewer, out map[string]DocDecision) (map[string]DocDecision, error) {
	if len(out) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(out))
	for id := range out {
		ids = append(ids, id)
	}
	ended, err := endedOn(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id := range ended {
		d, ok := out[id]
		if !ok {
			continue
		}
		if v.UUID == "" {
			d.Level, d.Unlocked, d.Reasons = level.None, level.None, nil
			out[id] = d
			continue
		}
		got, err := DecidePeople(ctx, id, []string{v.UUID})
		if err != nil {
			return nil, err
		}
		out[id] = got[v.UUID]
	}
	return out, nil
}
