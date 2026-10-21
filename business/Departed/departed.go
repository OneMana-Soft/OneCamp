// Package business (Departed) is what happens to a member's work once they
// have left the workspace (docs C6-2).
//
// Deactivating someone touches neither their docs nor their links: their docs
// stay theirs, and their guest links and published pages keep working. That is
// on purpose (a client's page shouldn't go dark the day someone leaves), so an
// admin decides: hand everything on to someone in the workspace, docs and
// links together, or turn their links off. Nothing here revokes on its own.
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
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
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
	recoverDoc = func(ctx context.Context, admin accessBusiness.Subject, docUUID, to string) error {
		_, err := accessBusiness.Recover(ctx, admin, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docUUID}, to, Reason)
		return err
	}
	moveLinks   = guestModel.HandOverLinks
	revokeLinks = guestModel.RevokeLiveByPerson
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
	var ids []string
	for _, p := range people {
		d, l := docs[strings.ToLower(p.ID.String())], links[p.ID]
		if helpers.IsDemoPerson(p.Email) || (d == 0 && l == 0) {
			continue
		}
		out = append(out, Person{UserUUID: p.ID.String(), LeftAt: p.LeftAt.UTC().Format("2006-01-02T15:04:05Z07:00"), Docs: d, Links: l})
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
	Links       int `json:"links"`
	Failed      int `json:"failed"`
	RoomsClosed int `json:"rooms_closed"`
}

// HandOver gives everything a person who left still owns to someone in the
// workspace: each doc by Recover (live or in the trash), then their live
// links and published pages, which keep working under the new name; their
// personal rooms close instead, being theirs. A doc
// that couldn't move is counted in Failed and left for the next run, as is
// any past the first handoverDocs.
//
// The docs are read once, and each moves by its own Recover: a recovery is
// recorded and shown to that doc's people one doc at a time, as an admin's
// recovery always is (helpers/testdata/store-loop-baseline.txt).
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
	ids, _, err := docsOwned(ctx, from.String(), handoverDocs)
	if err != nil {
		return m, err
	}
	for _, id := range ids {
		if err := recoverDoc(ctx, admin, id, to.String()); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Departed/HandOver doc %s: %+v", id, err)
			m.Failed++
			continue
		}
		m.Docs++
	}
	n, closed, err := moveLinks(ctx, from, to)
	if err != nil {
		return m, err
	}
	m.Links, m.RoomsClosed = int(n), int(closed)
	return m, nil
}

// TurnOffLinks revokes every live link and published page a person who left
// made or published, and answers how many.
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
	n, err := revokeLinks(ctx, who)
	return int(n), err
}

// Summary is a person's work in words: "12 docs and 3 links". Pure.
func Summary(docs, links int) string {
	part := func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	switch {
	case docs > 0 && links > 0:
		return part(docs, "doc", "docs") + " and " + part(links, "link", "links")
	case docs > 0:
		return part(docs, "doc", "docs")
	default:
		return part(links, "link", "links")
	}
}
