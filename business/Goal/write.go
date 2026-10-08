package business

import (
	"context"
	"errors"
	"time"

	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	goalModel "github.com/akashc777/OneCamp/models/postgres/Goal"
	"github.com/google/uuid"
)

// CheckParent approves a goal's place under parent in the tree as it stands:
// the parent is a live goal, the goal doesn't end up under itself or one of
// its own sub-goals, and the tree grows no deeper than MaxDepth. self is
// uuid.Nil for a new goal. Pure.
func CheckParent(nodes []goalModel.Node, self, parent uuid.UUID) error {
	parents := make(map[uuid.UUID]*uuid.UUID, len(nodes))
	children := map[uuid.UUID][]uuid.UUID{}
	for _, n := range nodes {
		parents[n.Id] = n.ParentId
		if n.ParentId != nil {
			children[*n.ParentId] = append(children[*n.ParentId], n.Id)
		}
	}
	if _, ok := parents[parent]; !ok {
		return refuse("That parent goal no longer exists.")
	}
	// The parent's level, counting from the top; a loop left in old data ends the walk.
	level := 0
	for at, seen := &parent, map[uuid.UUID]bool{}; at != nil && !seen[*at]; at = parents[*at] {
		if *at == self {
			return refuse("A goal can't sit under itself or one of its own sub-goals.")
		}
		seen[*at] = true
		level++
	}
	if level+height(children, self, map[uuid.UUID]bool{}) > MaxDepth {
		return refuse("Goals nest up to %d levels deep.", MaxDepth)
	}
	return nil
}

// height is how many levels a goal and its sub-goals take: 1 for a goal
// without any (or a new one). Pure.
func height(children map[uuid.UUID][]uuid.UUID, id uuid.UUID, seen map[uuid.UUID]bool) int {
	if seen[id] {
		return 0
	}
	seen[id] = true
	h := 0
	for _, c := range children[id] {
		h = max(h, height(children, c, seen))
	}
	return h + 1
}

// ownerOK checks the owner is a person in the workspace: not a bot, not
// someone from outside, not an account that was removed.
var ownerOK = func(ctx context.Context, owner uuid.UUID) error {
	nobody := refuse("Choose someone in the workspace to own the goal.")
	u, err := userDomain.GetDgraphUserInfoByUUID(ctx, owner.String())
	if err != nil {
		// That lookup fails for an id nobody has; only a real fault is one.
		if found, lerr := userDomain.ResolveUserDisplays(ctx, []string{owner.String()}); lerr == nil && found[owner.String()].Uuid == "" {
			return nobody
		}
		return err
	}
	if u == nil || u.Uuid == "" || u.IsBot || u.IsExternal || (u.DeletedAt != nil && u.DeletedAt.After(time.Unix(0, 0))) {
		return nobody
	}
	return nil
}

// projectsOK checks the reader is in each project, so a goal never names, or
// counts, a project its editor can't see.
var projectsOK = func(ctx context.Context, r Reader, ids []uuid.UUID, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	got, err := projectDomain.GetDgraphProjectsProgress(ctx, ids, r.DgraphUID, today(now))
	if err != nil {
		return err
	}
	for _, id := range ids {
		p, ok := got[id.String()]
		if !ok || !p.Visible() {
			return refuse("You can only add projects you're in.")
		}
		if p.Archived() {
			return refuse("%s is archived. Restore it to add it to a goal.", p.Name)
		}
	}
	return nil
}

// parentEditable checks the reader may change what counts towards a goal:
// putting a goal under it, or taking one out, moves its progress. A parent
// that's gone is no obstacle to leaving it, but can't be joined.
func parentEditable(r Reader, id uuid.UUID, joining bool) error {
	p, err := goalModel.Get(id)
	if err != nil {
		return err
	}
	switch {
	case p == nil && joining:
		return refuse("That parent goal no longer exists.")
	case p == nil:
		return nil
	case !r.CanEdit(p) && joining:
		return refuse("Only the owner of %q, whoever made it, or a workspace admin can put a goal under it.", p.Title)
	case !r.CanEdit(p):
		return refuse("Only the owner of %q, whoever made it, or a workspace admin can take a goal out from under it.", p.Title)
	}
	return nil
}

// Create makes a goal as the reader, linking its first projects, and
// returns it as the list shows it.
func Create(ctx context.Context, r Reader, in Input, now time.Time) (*Summary, error) {
	g, projects, err := Check(in)
	if err != nil {
		return nil, err
	}
	if err := ownerOK(ctx, g.OwnerUUID); err != nil {
		return nil, err
	}
	if err := projectsOK(ctx, r, projects, now); err != nil {
		return nil, err
	}
	g.CreatedBy = r.UUID
	var check func([]goalModel.Node) error
	if g.ParentId != nil {
		parent := *g.ParentId
		if err := parentEditable(r, parent, true); err != nil {
			return nil, err
		}
		check = func(nodes []goalModel.Node) error { return CheckParent(nodes, uuid.Nil, parent) }
	}
	made, err := goalModel.Create(*g, projects, check)
	if err != nil {
		return nil, err
	}
	return summaryOf(ctx, r, made.Id, now)
}

// summaryOf is one goal as the list shows it, read fresh.
func summaryOf(ctx context.Context, r Reader, id uuid.UUID, now time.Time) (*Summary, error) {
	b, err := load(ctx, r, now)
	if err != nil {
		return nil, err
	}
	if err := b.ensure(ctx, id); err != nil {
		return nil, err
	}
	g := b.goals[id]
	s := b.summary(g, people(ctx, []*goalModel.Goal{g}))
	return &s, nil
}

// editable is the live goal, if the reader may change it.
func editable(r Reader, id uuid.UUID) (*goalModel.Goal, error) {
	g, err := goalModel.Get(id)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, ErrNotFound
	}
	if !r.CanEdit(g) {
		return nil, ErrNotYours
	}
	return g, nil
}

// Edit changes a goal's title, description, owner, dates, parent and measure.
// Its projects are added and removed on their own.
func Edit(ctx context.Context, r Reader, id uuid.UUID, in Input, now time.Time) (*Summary, error) {
	cur, err := editable(r, id)
	if err != nil {
		return nil, err
	}
	in.ProjectUUIDs = nil
	// The number moves with check-ins: an edit that doesn't give it keeps
	// where it is, rather than where it was when the editor opened.
	if in.Measure == goalModel.MeasureNumber && in.CurrentValue == nil && cur.Measure == goalModel.MeasureNumber {
		in.CurrentValue = cur.CurrentValue
	}
	g, _, err := Check(in)
	if err != nil {
		return nil, err
	}
	g.Id = id
	if g.OwnerUUID != cur.OwnerUUID {
		if err := ownerOK(ctx, g.OwnerUUID); err != nil {
			return nil, err
		}
	}
	parentChanged := (g.ParentId == nil) != (cur.ParentId == nil) || (g.ParentId != nil && *g.ParentId != *cur.ParentId)
	if parentChanged {
		if cur.ParentId != nil {
			if err := parentEditable(r, *cur.ParentId, false); err != nil {
				return nil, err
			}
		}
		if g.ParentId != nil {
			if err := parentEditable(r, *g.ParentId, true); err != nil {
				return nil, err
			}
		}
	}
	check := func(nodes []goalModel.Node) error {
		if g.ParentId == nil {
			return nil
		}
		return CheckParent(nodes, id, *g.ParentId)
	}
	updated, err := goalModel.Update(*g, parentChanged, check)
	if err != nil {
		return nil, err
	}
	if updated == nil {
		return nil, ErrNotFound
	}
	return summaryOf(ctx, r, id, now)
}

// Delete removes a goal; its sub-goals move up to its parent.
func Delete(r Reader, id uuid.UUID) error {
	if _, err := editable(r, id); err != nil {
		return err
	}
	ok, err := goalModel.Delete(id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// Reopen opens a closed goal again, for more work or a closing made in error.
func Reopen(r Reader, id uuid.UUID) error {
	g, err := editable(r, id)
	if err != nil {
		return err
	}
	if g.Status == goalModel.StatusOpen {
		return nil
	}
	_, err = goalModel.Reopen(id)
	return err
}

// LinkProject adds a project the reader is in to a goal.
func LinkProject(ctx context.Context, r Reader, id, projectID uuid.UUID, now time.Time) error {
	if _, err := editable(r, id); err != nil {
		return err
	}
	if err := projectsOK(ctx, r, []uuid.UUID{projectID}, now); err != nil {
		return err
	}
	_, err := goalModel.LinkProject(id, projectID, r.UUID, MaxProjects)
	if errors.Is(err, goalModel.ErrTooManyProjects) {
		return refuse("A goal can have up to %d projects. Remove one to add another.", MaxProjects)
	}
	return err
}

// UnlinkProject takes a project off a goal. The project itself is untouched.
func UnlinkProject(r Reader, id, projectID uuid.UUID) error {
	if _, err := editable(r, id); err != nil {
		return err
	}
	_, err := goalModel.UnlinkProject(id, projectID)
	return err
}
