package business

// A page's space is one of the sources of who may open it: everyone in a
// project's or a team's space reaches every page in it (business/Doc
// docAccessDecide.go). Moving a page into a space shares it with that space's
// people, and moving it out takes it from them. So a move that changes a
// page's space is a change to its sharing, and takes what Apply takes:
//   - full access to the page, and to every page under it that the move takes
//     along (the tree decides those, under its lock: business/Tree Move);
//   - into a space, being in it, as a member or an admin of the project or
//     team, or being a workspace admin (EnterSpace, which a page made at the
//     top of a space takes too).
//
// What follows the move is what follows any change to sharing (Apply): the
// indexes are written again (I13), the change is told (I14), and the live
// sockets of the people whose level moved are closed (I10).

import (
	"context"
	"errors"
	"strings"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
)

var (
	// ErrSpaceNeedsFull is a move into another space, or out of one, by
	// someone without full access to the page or to a page under it that the
	// move takes along.
	ErrSpaceNeedsFull = errors.New("access: moving a page to another space needs full access to it and to every page under it")
	// ErrNotInSpace is a page put into a space by someone who isn't in it.
	// A space that isn't there answers the same, to anyone but a workspace
	// admin, so the answer never says whether a project or team exists.
	ErrNotInSpace = errors.New("access: pages go only into a project or team you're in")
	// ErrNoSuchSpace is a space that isn't there, to a workspace admin.
	ErrNoSuchSpace = docBusiness.ErrNoSuchSpace
)

// SpaceMove is a page cleared to move into another space, or out of the one
// it is in: what the move's preview and what follows the move need.
type SpaceMove struct {
	who Subject
	res Resource
	// to is the space it goes into; the zero Space takes it out of any.
	to     docBusiness.Space
	before *docBusiness.Sharing
}

// PrepareSpaceMove clears who to move res into a space (kind "project" or
// "team"), or with kind "" out of the one it is in: full access to res, and
// into a space, being in it or a workspace admin acting themselves. It reads
// res's sharing as it is now, which the move is explained against.
func PrepareSpaceMove(ctx context.Context, who Subject, res Resource, kind, spaceUUID string) (*SpaceMove, error) {
	if _, err := Open(ctx, who, res, LevelFull); err != nil {
		if errors.Is(err, ErrForbidden) {
			return nil, ErrSpaceNeedsFull
		}
		return nil, err
	}
	m := &SpaceMove{who: who, res: res}
	if kind != "" {
		sp, err := EnterSpace(ctx, who, kind, spaceUUID)
		if err != nil {
			return nil, err
		}
		m.to = sp
	}
	before, err := docBusiness.ReadSharing(ctx, res.UUID)
	if err != nil {
		return nil, err
	}
	m.before = before
	return m, nil
}

// EnterSpace is the project's or team's space (kind "project" or "team"),
// with who is in it, cleared for who to put pages into: they're in it, as a
// member or an admin, or they're a workspace admin acting themselves. A space
// that isn't there is ErrNotInSpace, so the answer never says whether a
// project or team exists, except to a workspace admin (ErrNoSuchSpace).
func EnterSpace(ctx context.Context, who Subject, kind, spaceUUID string) (docBusiness.Space, error) {
	sp, err := docBusiness.ReadSpace(ctx, kind, spaceUUID)
	switch {
	case errors.Is(err, docBusiness.ErrNoSuchSpace) && !who.ActingAdmin():
		return docBusiness.Space{}, ErrNotInSpace
	case err != nil:
		return docBusiness.Space{}, err
	}
	if err := mayEnter(who, sp); err != nil {
		return docBusiness.Space{}, err
	}
	return sp, nil
}

// mayEnter says whether who may put pages into sp: its members and admins
// may, and a workspace admin acting themselves. Pure.
func mayEnter(who Subject, sp docBusiness.Space) error {
	if sp.Members[strings.ToLower(who.UserUUID)] || who.ActingAdmin() {
		return nil
	}
	return ErrNotInSpace
}

// Spaces are the space the page goes into and the one it leaves: the zero
// Space for none.
func (m *SpaceMove) Spaces() (to, from docBusiness.Space) {
	return m.to, m.before.Space
}

// Explain is what the move would do to who can open the page, before it is
// made: with adopt (nil for none), what taking its new parent's sharing does
// too (AdoptChange). pages is how many pages the move changes.
func (m *SpaceMove) Explain(ctx context.Context, adopt *Change, pages int) (Explanation, error) {
	after := m.before
	if adopt != nil {
		sc, err := sharingChangeOf(m.before, *adopt)
		if err != nil {
			return Explanation{}, err
		}
		after, err = m.before.After(ctx, sc)
		if errors.Is(err, docBusiness.ErrNoSuchPrincipal) {
			return Explanation{}, ErrNoSuchPrincipal
		}
		if err != nil {
			return Explanation{}, err
		}
	}
	return explanationOf(docBusiness.Explain(m.before, after.InSpace(m.to)), pages), nil
}

// Done does what follows the move once it has landed, for the pages whose
// space it changed (Moved.Spaced): their indexes are written again, the
// change is told (doc.access_changed, and doc.shared to the people who can
// open the page now and couldn't before), and the live sockets of the people
// whose level on the page moved are closed, and everyone's on the pages under
// it, whose people differ from the page's (as Apply does). It answers the
// move's change of space, which its doc.moved tells the owner of.
func (m *SpaceMove) Done(ctx context.Context, spaced []string) *docBusiness.SpaceChange {
	change := &docBusiness.SpaceChange{From: m.before.Space, To: m.to}
	if m.before.Owner != nil {
		change.Owner = strings.ToLower(m.before.Owner.Uuid)
	}
	if len(spaced) == 0 {
		return change
	}
	after, err := docBusiness.ReadSharing(ctx, m.res.UUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Access SpaceMove %s: its sharing after the move wasn't read, so it is explained as the move meant it: %+v", m.res.UUID, err)
		after = m.before.InSpace(m.to)
	}
	var under []string
	for _, id := range spaced {
		if !strings.EqualFold(id, m.res.UUID) {
			under = append(under, id)
		}
	}
	actor := docBusiness.Actor{UserUUID: m.who.UserUUID, AgentUUID: m.who.AgentUUID}
	data := map[string]string{"space": change.To.Ref(), "was": change.From.Ref()}
	docBusiness.AccessChanged(ctx, []string{m.res.UUID}, actor, resourceEventBusiness.DocAccessChanged, docBusiness.Explain(m.before, after), data)
	if len(under) > 0 {
		docBusiness.AccessChanged(ctx, under, actor, resourceEventBusiness.DocAccessChanged, docBusiness.Explained{Everyone: &docBusiness.Shift{}}, data)
	}
	return change
}
