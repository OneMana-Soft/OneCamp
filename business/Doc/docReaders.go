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
// ResolvePeople); business/Comment and business/Notification, which this
// package imports and so can't import it, are handed the rule when the server
// starts (Readers, and ReadersOfMany for many docs).

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	businessComment "github.com/akashc777/OneCamp/business/Comment"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	accessModels "github.com/akashc777/OneCamp/models/postgres/Access"
	"github.com/google/uuid"
)

// errNoStore is the grants that end being unreadable for want of a database:
// who may read the doc can't be said, so nobody is told.
var errNoStore = errors.New("doc readers: the grants that end can't be read")

// Seams for tests: the doc's sharing, the grants on it that end (of one doc
// and of many), and the clock.
var (
	readersSharing  = ReadSharing
	readersSharings = ReadSharings
	readersEnding   = func(ctx context.Context, docUUID string) ([]*accessModels.Expiry, error) {
		id, err := uuid.Parse(docUUID)
		if err != nil {
			return nil, ErrNotFound
		}
		on, err := readersEndingOn(ctx, []uuid.UUID{id})
		if err != nil {
			return nil, err
		}
		return on[id], nil
	}
	readersEndingOn = func(ctx context.Context, docs []uuid.UUID) (map[uuid.UUID][]*accessModels.Expiry, error) {
		if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
			return nil, errNoStore
		}
		return accessModels.ListExpiryOn(ctx, "doc", docs)
	}
	readersNow = time.Now
	// dueDocGrants is the docs holding a grant that ends by `by`, with the
	// earliest such end, at most limit of them (domain.DocGrantEnds).
	dueDocGrants = func(ctx context.Context, by time.Time, limit int) (map[string]time.Time, error) {
		if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
			// No database: no grant was ever given until a date.
			return map[string]time.Time{}, nil
		}
		return domain.DocGrantEnds(ctx, by, limit)
	}
	// expiryWrites is the generation of access_grant_expiry
	// (accessModels.ExpiryWrites).
	expiryWrites = accessModels.ExpiryWrites
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
	notificationBusiness.RegisterDocReaders(ReadersOfMany)
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
	decideEach(withoutEnded(s, ends, readersNow()), userUUIDs, out)
	return out, nil
}

// decideEach puts each person's decision on the doc whose sharing is s into
// out, keyed as given (an empty id is left out). Pure.
func decideEach(s *Sharing, userUUIDs []string, out map[string]DocDecision) {
	for _, id := range userUUIDs {
		if id != "" {
			out[id] = s.DecisionOf(id)
		}
	}
}

// Readers is, of the people named, who may read the doc now (view or more by
// DecidePeople), keyed as given; false for the rest.
func Readers(ctx context.Context, docUUID string, userUUIDs []string) (map[string]bool, error) {
	decided, err := DecidePeople(ctx, docUUID, userUUIDs)
	if err != nil {
		return nil, err
	}
	return reading(decided), nil
}

// reading is who of the decided may read the doc: view or more. Pure.
func reading(decided map[string]DocDecision) map[string]bool {
	out := make(map[string]bool, len(decided))
	for id, d := range decided {
		out[id] = d.Level >= level.View
	}
	return out
}

// ReadersOfMany is Readers for many docs: for each doc (by uuid, keyed as
// given), which of the people named may read it now, by the same rule. A doc
// that isn't there, or isn't a uuid, is read by nobody. One read of the docs'
// sharing and one of the grants on them that end, however many docs there
// are. The emails asked again at their send ask it (business/Notification
// RegisterDocReaders).
func ReadersOfMany(ctx context.Context, people map[string][]string) (map[string]map[string]bool, error) {
	out := make(map[string]map[string]bool, len(people))
	docs, ids := make([]string, 0, len(people)), make([]uuid.UUID, 0, len(people))
	for doc := range people {
		out[doc] = map[string]bool{}
		if id, err := uuid.Parse(doc); err == nil {
			docs, ids = append(docs, id.String()), append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	sharings, err := readersSharings(ctx, docs)
	if err != nil {
		return nil, err
	}
	ends, err := readersEndingOn(ctx, ids)
	if err != nil {
		return nil, err
	}
	now := readersNow()
	for doc, who := range people {
		id, err := uuid.Parse(doc)
		if err != nil || sharings[id.String()] == nil {
			continue
		}
		decided := make(map[string]DocDecision, len(who))
		decideEach(withoutEnded(sharings[id.String()], ends[id], now), who, decided)
		out[doc] = reading(decided)
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
	read := endedOn
	if opening, _ := ctx.Value(openingDoc{}).(bool); opening {
		read = endedForOpen
	}
	ended, err := read(ctx, ids)
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

// dueGrantsMaxAge is how long a doc open keeps the doc grants due to end,
// and how far ahead it reads them: any end that passes while they are kept
// is among them.
const dueGrantsMaxAge = 15 * time.Second

// dueGrantsKept is at most how many due docs are kept; with more, each open
// reads its own doc's ended grants (endedOn), as before.
const dueGrantsKept = 500

// keptDue is the doc grants due to end as a doc open read them: when
// (readersNow), at which generation of the grants' ends (expiryWrites), and
// each due doc's earliest end; tooMany when there were more than are kept.
type keptDue struct {
	generation uint64
	at         time.Time
	ends       map[string]time.Time
	tooMany    bool
}

// dueGrants keeps one keptDue for every doc open in this process (H6 A2-2).
// What invalidates it: any write of a grant's end in this process (every
// writer of access_grant_expiry bumps accessModels.ExpiryWrites), and its age
// (dueGrantsMaxAge). What a stale one could expose: an end set or moved on
// another server instance (whose writes this process doesn't count) is
// honoured by this instance's doc opens 15 s late at most; writes and every
// other decision never read it (OpeningDoc).
var dueGrants = helpers.NewTTLCache[keptDue](dueGrantsMaxAge)

type openingDoc struct{}

// OpeningDoc marks ctx as a doc open's read (GetDocPage): whether a grant of
// the doc has ended is then answered from the kept due grants
// (endedForOpen), with no read of the doc's own when none is due. Only the
// doc open sets it: every write, and every other decision, reads the ended
// grants fresh (endedOn).
func OpeningDoc(ctx context.Context) context.Context {
	return context.WithValue(ctx, openingDoc{}, true)
}

// endedForOpen is endedOn for a doc open, from the doc grants due to end
// within dueGrantsMaxAge: read once, then kept while this process wrote no
// grant's end and for at most dueGrantsMaxAge. One clock, readersNow, sets
// both how far ahead they are read and whether an end has passed. The
// generation is read before the grants, so a write during the read leaves
// them stale at once. A failed read fails the open and keeps nothing.
func endedForOpen(ctx context.Context, docUUIDs []string) (map[string]bool, error) {
	now, generation := readersNow(), expiryWrites()
	k, ok := dueGrants.Get("doc")
	if !ok || k.generation != generation || now.Before(k.at) || now.Sub(k.at) >= dueGrantsMaxAge {
		ends, err := dueDocGrants(ctx, now.Add(dueGrantsMaxAge), dueGrantsKept+1)
		if err != nil {
			return nil, err
		}
		k = keptDue{generation: generation, at: now, ends: ends, tooMany: len(ends) > dueGrantsKept}
		if k.tooMany {
			k.ends = nil
		}
		dueGrants.Set("doc", k)
	}
	if k.tooMany {
		return endedOn(ctx, docUUIDs)
	}
	out := map[string]bool{}
	for _, id := range docUUIDs {
		if end, ok := k.ends[strings.ToLower(id)]; ok && !now.Before(end) {
			out[id] = true
		}
	}
	return out, nil
}
