// Package business (Departed) is what happens to a member's work once they
// have left the workspace (docs C6-2).
//
// Deactivating someone touches neither their docs nor their links: their docs
// stay theirs, and their guest links and published pages keep working. That is
// on purpose (a client's page shouldn't go dark the day someone leaves), so an
// admin decides: hand everything on to someone in the workspace, docs and
// links together, or turn their links off. Nothing here revokes on its own.
//
// A departed member's boards go with their docs (miro-32), each by the same
// Recover, which hands a board on by the hand-on business/Board registers.
//
// It builds on what exists: a doc changes hands by business/Access Recover,
// one at a time, recorded and shown to the doc's people as an admin's
// recovery always is; links move by the grant model's HandOverLinks. A sweep
// tells each admin once, in their Inbox, about each person who left with work
// still under their name; the demo's own people never reach a real person's
// Inbox (helpers.IsDemoPerson).
package business

import (
	"context"
	"errors"
	"fmt"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	boardDomain "github.com/akashc777/OneCamp/domain/Board"
	departedDomain "github.com/akashc777/OneCamp/domain/Departed"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	departedModel "github.com/akashc777/OneCamp/models/postgres/Departed"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
)

// Errors a handover answers.
var (
	ErrNotAdmin    = errors.New("departed: only a workspace admin acting themselves can do that")
	ErrNotDeparted = errors.New("departed: that person hasn't left the workspace")
	ErrNotAMember  = errors.New("departed: work can be handed only to someone in this workspace")
)

// Reason is what each doc's recovery records, which its people see.
const Reason = "Left the workspace"

// Limits: how many people who left an admin sees at once, and how many docs
// one handover moves (it can be run again for the rest).
const (
	listLimit    = 100
	handoverDocs = 5000
)

// Person is someone who left with work still under their name.
type Person struct {
	UserUUID string `json:"user_uuid"`
	Name     string `json:"name"`
	LeftAt   string `json:"left_at"`
	Docs     int    `json:"docs"`
	Boards   int    `json:"boards"`
	Links    int    `json:"links"`
}

// Seams for tests: who left, their docs and links, names, and the moves.
var (
	departedPeople = departedModel.ListDeparted
	departedOne    = departedModel.Departed
	activeMember   = departedModel.ActiveMember
	docsOwned      = docDomain.DocsOwnedBy
	docCounts      = docDomain.DocsOwnedCounts
	linkCounts     = guestModel.CountLiveByPeople
	names          = func(ctx context.Context, ids []string) (map[string]string, error) {
		people, err := userDomain.GetPeopleByUUIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
		out := map[string]string{}
		for _, p := range people {
			if p != nil {
				out[strings.ToLower(p.Uuid)] = p.DisplayName()
			}
		}
		return out, nil
	}
	// recoverOne is an admin's recovery of one doc or board to someone, for
	// the reason a handover gives (Recover; a doc's and a board's are the same).
	recoverOne = func(ctx context.Context, admin accessBusiness.Subject, kind, id, to string) error {
		_, err := accessBusiness.Recover(ctx, admin, accessBusiness.Resource{Kind: kind, UUID: id}, to, Reason)
		return err
	}
	boardsOwned = boardDomain.BoardsOwnedBy
	boardCounts = boardDomain.BoardsOwnedCounts
	moveLinks   = guestModel.HandOverLinks
	revokeLinks = guestModel.RevokeLiveByPerson
	// endSessions closes the live views guests opened by links turned off.
	endSessions = guestBusiness.EndLiveSessions
	// docTies reads which of their docs are a call's notes, and the
	// conversation each was in (V-1209); handoverPeople who of those
	// conversations' people is still here. One read each per handover.
	docTies        = recordingDomain.NotesDocTies
	handoverPeople = departedDomain.HandoverPeople
)

// List is the people who left with docs or live links still under their
// name, the most recent first. The demo's own people are left out. Their docs
// and links are counted for everyone at once: one read each.
func List(ctx context.Context) ([]Person, error) {
	people, err := departedPeople(ctx, listLimit)
	if err != nil {
		return nil, err
	}
	gone := make([]uuid.UUID, 0, len(people))
	for _, p := range people {
		if !helpers.IsDemoPerson(p.Email) {
			gone = append(gone, p.ID)
		}
	}
	out := []Person{}
	if len(gone) == 0 {
		return out, nil
	}
	docs, err := docCounts(ctx, gone)
	if err != nil {
		return nil, err
	}
	links, err := linkCounts(ctx, gone)
	if err != nil {
		return nil, err
	}
	boards, err := boardCounts(ctx, gone)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, p := range people {
		d, b, l := docs[strings.ToLower(p.ID.String())], boards[strings.ToLower(p.ID.String())], links[p.ID]
		if helpers.IsDemoPerson(p.Email) || (d == 0 && b == 0 && l == 0) {
			continue
		}
		out = append(out, Person{UserUUID: p.ID.String(), LeftAt: p.LeftAt.UTC().Format("2006-01-02T15:04:05Z07:00"), Docs: d, Boards: b, Links: l})
		ids = append(ids, p.ID.String())
	}
	if len(ids) > 0 {
		if named, err := names(ctx, ids); err == nil {
			for i := range out {
				out[i].Name = named[strings.ToLower(out[i].UserUUID)]
			}
		}
	}
	for i := range out {
		if out[i].Name == "" {
			out[i].Name = "Someone who left"
		}
	}
	return out, nil
}

// Moved is what a handover moved, and the personal rooms it closed (a room
// ends when its owner leaves, D-CL-35).
type Moved struct {
	Docs        int `json:"docs"`
	Boards      int `json:"boards"`
	Links       int `json:"links"`
	Failed      int `json:"failed"`
	RoomsClosed int `json:"rooms_closed"`
	// Kept is the title of each private conversation's notes doc that
	// stayed with them: nobody is left in its conversation to take it.
	Kept []string `json:"kept,omitempty"`
}

// recipientOf is who a private conversation's notes doc goes to: to when
// they are in its conversation, else the first of its people still here;
// false when nobody is. Pure.
func recipientOf(tie recordingDomain.NotesDocTie, to string, active map[uuid.UUID]bool, eventPeople map[uuid.UUID][]uuid.UUID) (string, bool) {
	if tie.Event != "" {
		e, err := uuid.Parse(tie.Event)
		if err != nil {
			return "", false
		}
		for _, p := range eventPeople[e] {
			if p.String() == to {
				return to, true
			}
		}
		if people := eventPeople[e]; len(people) > 0 {
			return people[0].String(), true
		}
		return "", false
	}
	if tie.ToIn {
		return to, true
	}
	for _, p := range tie.Order {
		if id, err := uuid.Parse(p); err == nil && active[id] {
			return id.String(), true
		}
	}
	return "", false
}

// privateTies reads which of ids are a private conversation's call notes,
// and who of each conversation's people is still here: one read of the ties
// and one of the people, whatever the number of docs.
func privateTies(ctx context.Context, ids []string, to string) (map[string]recordingDomain.NotesDocTie, map[uuid.UUID]bool, map[uuid.UUID][]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil, nil, nil
	}
	ties, err := docTies(ctx, ids, to)
	if err != nil {
		return nil, nil, nil, err
	}
	var people, events []uuid.UUID
	for id, tie := range ties {
		if tie.Public {
			delete(ties, id)
			continue
		}
		if e, err := uuid.Parse(tie.Event); err == nil {
			events = append(events, e)
		}
		for _, p := range tie.Order {
			if u, err := uuid.Parse(p); err == nil {
				people = append(people, u)
			}
		}
	}
	if len(ties) == 0 {
		return nil, nil, nil, nil
	}
	active, eventPeople, err := handoverPeople(ctx, people, events)
	if err != nil {
		return nil, nil, nil, err
	}
	return ties, active, eventPeople, nil
}

// HandOver gives everything a person who left still owns to someone in the
// workspace: each doc by Recover (live or in the trash), then their live
// links and published pages, which keep working under the new name; their
// personal rooms close instead, being theirs. A doc
// that couldn't move is counted in Failed and left for the next run, as is
// any past the first handoverDocs.
//
// A doc that is a private channel's, DM's or event's call notes (its recap
// and full transcript) goes to the person named only when they are in that
// conversation, else to its maker, a moderator, a participant or the
// organiser still here; with nobody left it stays theirs and the admin is
// told its title (Kept), nothing more (V-1209).
//
// The docs, then the boards, are read once, and each moves by its own
// Recover: a recovery is recorded and shown to its people one at a time, as
// an admin's recovery always is (helpers/testdata/store-loop-baseline.txt).
func HandOver(ctx context.Context, admin accessBusiness.Subject, from, to uuid.UUID) (Moved, error) {
	var m Moved
	if !admin.ActingAdmin() {
		return m, ErrNotAdmin
	}
	gone, err := departedOne(ctx, from)
	if err != nil {
		return m, err
	}
	if gone == nil {
		return m, ErrNotDeparted
	}
	ok, _, err := activeMember(ctx, to)
	if err != nil {
		return m, err
	}
	if !ok {
		return m, ErrNotAMember
	}
	// Their docs, then their boards, each by its own recovery; one that
	// can't move is counted in Failed and left for the next run. A private
	// conversation's call notes go to someone still in it, never to anyone
	// outside it (V-1209); with nobody left they stay. Each read is once.
	docIDs, _, err := docsOwned(ctx, from.String(), handoverDocs)
	if err != nil {
		return m, err
	}
	ties, active, eventPeople, err := privateTies(ctx, docIDs, to.String())
	if err != nil {
		return m, err
	}
	boardIDs, _, err := boardsOwned(ctx, from.String(), handoverDocs)
	if err != nil {
		return m, err
	}
	for _, kind := range []struct {
		ids   []string
		name  string
		moved *int
	}{{docIDs, accessBusiness.KindDoc, &m.Docs}, {boardIDs, boardBusiness.KindBoard, &m.Boards}} {
		for _, id := range kind.ids {
			recipient := to.String()
			if tie, tied := ties[id]; tied && kind.name == accessBusiness.KindDoc {
				r, ok := recipientOf(tie, recipient, active, eventPeople)
				if !ok {
					m.Kept = append(m.Kept, tie.Title)
					continue
				}
				recipient = r
			}
			if err := recoverOne(ctx, admin, kind.name, id, recipient); err != nil {
				helpers.LogErrorWithContext(ctx, "business/Departed/HandOver %s: %+v", id, err)
				m.Failed++
				continue
			}
			*kind.moved++
		}
	}
	n, closed, err := moveLinks(ctx, from, to)
	if err != nil {
		return m, err
	}
	m.Links, m.RoomsClosed = int(n), int(closed)
	return m, nil
}

// TurnOffLinks revokes every live link and published page a person who left
// made or published, and answers how many. A guest with one open on a doc or
// board loses it at once.
func TurnOffLinks(ctx context.Context, admin accessBusiness.Subject, who uuid.UUID) (int, error) {
	if !admin.ActingAdmin() {
		return 0, ErrNotAdmin
	}
	gone, err := departedOne(ctx, who)
	if err != nil {
		return 0, err
	}
	if gone == nil {
		return 0, ErrNotDeparted
	}
	ids, err := revokeLinks(ctx, who)
	if err != nil {
		return 0, err
	}
	endSessions(ids...)
	return len(ids), nil
}

// Work is a person's work in words: "12 docs, 2 boards and 3 links", what
// they have of each. Pure.
func Work(docs, boards, links int) string {
	part := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	var parts []string
	for _, p := range []struct {
		n         int
		one, many string
	}{{docs, "doc", "docs"}, {boards, "board", "boards"}, {links, "link", "links"}} {
		if p.n > 0 {
			parts = append(parts, part(p.n, p.one, p.many))
		}
	}
	switch len(parts) {
	case 0:
		return part(links, "link", "links")
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}
