package businness

// A doc's whole sharing, read for the sharing panel and for working out what a
// change would do before it is made ("3 people lose access"): who it is shared
// with at each level, its teams and who is in them, its space and who is in
// it, its general access and its lock. Levels come from the same rule as
// every check (docAccessDecide.go), applied from the doc's side.

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// GeneralAccess is who, besides the people and teams a doc is shared with, may
// open it, and how much.
type GeneralAccess struct {
	// Private: only the people and teams it is shared with, and its space's
	// members, may open it. Otherwise everyone in the workspace may.
	Private bool
	// Level is what that general audience gets: everyone in the workspace when
	// the doc isn't private, its space's members when it is in a space. View,
	// comment or edit; a space's members never get less than view.
	Level level.Level
}

// TeamShare is a team a doc is shared with.
type TeamShare struct {
	UUID    string
	Name    string
	Level   level.Level
	Members []string // the uuids of its members and admins
}

// Sharing is a doc's whole sharing.
type Sharing struct {
	DocUUID string
	Title   string
	Owner   *dgraphStruct.DgraphUser
	Maker   *dgraphStruct.DgraphUser
	// People is everyone it is shared with by name, at their level (full,
	// edit, comment or view); never its owner, who has full by owning it.
	People map[string]level.Level
	// Names are the people it names (its owner, its maker, People), by uuid.
	Names   map[string]*dgraphStruct.DgraphUser
	Teams   map[string]*TeamShare
	General GeneralAccess
	// Space is the project's or team's space it is in, with who is in it: the
	// zero Space for none.
	Space    Space
	Lock     string
	LockedBy *dgraphStruct.DgraphUser
	LockedAt *time.Time
	Deleted  bool
	Archived bool
}

// flagsOf is general access as a doc stores it: private, "everyone may
// comment" and "everyone may edit". Only a general level of comment sets the
// first, which also lets everyone who can read the doc comment (as it always
// has); edit sets the second alone, so a person given view by name stays a
// viewer.
func flagsOf(g GeneralAccess) (private, comment, edit bool) {
	return g.Private, g.Level == level.Comment, g.Level >= level.Edit
}

// GeneralOf is a doc's general access, from its privacy and its "everyone may
// comment" and "everyone may edit" settings.
func GeneralOf(doc *dgraphStruct.DgraphDoc) GeneralAccess {
	return GeneralAccess{Private: domain.IsPrivate(doc), Level: GeneralLevel(doc)}
}

// ReadSharing reads a doc's whole sharing, deleted or not. ErrNotFound when
// there's no such doc.
func ReadSharing(ctx context.Context, docUUID string) (*Sharing, error) {
	acl, err := domain.GetDocACL(ctx, docUUID)
	if err != nil {
		return nil, err
	}
	d := acl.Doc
	s := &Sharing{
		DocUUID: d.Uuid, Title: d.Title, Maker: d.CreatedBy, Owner: domain.OwnerOf(d),
		People: map[string]level.Level{}, Names: map[string]*dgraphStruct.DgraphUser{},
		Teams: map[string]*TeamShare{}, General: GeneralOf(d),
		Space:    spaceNamed(d.SpaceKind, d.SpaceUUID, acl.SpaceName, acl.SpaceMembers),
		LockedBy: d.LockedBy, LockedAt: d.LockedAt,
		Deleted: IsDeleted(d), Archived: DecideDoc(d, Viewer{}).Archived,
	}
	switch d.LockLevel {
	case level.LockSoft, level.LockHard:
		s.Lock = d.LockLevel
	}
	for _, u := range []*dgraphStruct.DgraphUser{s.Owner, s.Maker} {
		if u != nil && u.Uuid != "" {
			s.Names[strings.ToLower(u.Uuid)] = u
		}
	}
	owner := ""
	if s.Owner != nil {
		owner = strings.ToLower(s.Owner.Uuid)
	}
	// Someone on two lists keeps the higher level (lists are read weakest
	// first); the owner is on none.
	for _, l := range []struct {
		users []*dgraphStruct.DgraphUser
		lvl   level.Level
	}{{d.ReadingUser, level.View}, {d.CommentingUser, level.Comment}, {d.EditingUser, level.Edit}, {d.FullAccessUsers, level.Full}} {
		for _, u := range l.users {
			if u == nil || u.Uuid == "" {
				continue
			}
			id := strings.ToLower(u.Uuid)
			s.Names[id] = u
			if id != owner && s.People[id] < l.lvl {
				s.People[id] = l.lvl
			}
		}
	}
	for _, l := range []struct {
		teams []*dgraphStruct.DgraphTeam
		lvl   level.Level
	}{{d.TeamViewers, level.View}, {d.TeamCommenters, level.Comment}, {d.TeamEditors, level.Edit}} {
		for _, t := range l.teams {
			if t == nil || t.Uuid == "" {
				continue
			}
			id := strings.ToLower(t.Uuid)
			ts := s.Teams[id]
			if ts == nil {
				ts = &TeamShare{UUID: id, Name: t.Name, Members: domain.PeopleOf(t.Members, t.Admins)}
				s.Teams[id] = ts
			}
			if ts.Level < l.lvl {
				ts.Level = l.lvl
			}
		}
	}
	return s, nil
}

// rowFor is the doc as a read of it for the person would carry it: what
// DecideDoc decides by. The same rule, from the doc's side.
func (s *Sharing) rowFor(userUUID string) *dgraphStruct.DgraphDoc {
	id := strings.ToLower(userUUID)
	priv, comment, edit := flagsOf(s.General)
	row := &dgraphStruct.DgraphDoc{
		Uuid: s.DocUUID, IsPrivate: &priv, PublicComment: &comment, PublicEdit: &edit,
		LockLevel: s.Lock, SpaceKind: s.Space.Kind, SpaceUUID: s.Space.UUID, Owner: s.Owner, CreatedBy: s.Maker,
	}
	if s.Deleted {
		gone := time.Unix(1, 0).UTC()
		row.DeletedAt = &gone
	}
	switch s.People[id] {
	case level.Full:
		row.HasFullAccess = 1
	case level.Edit:
		row.HasEditAccess = 1
	case level.Comment:
		row.HasCommentAccess = 1
	case level.View:
		row.HasReadAccess = 1
	}
	for _, t := range s.Teams {
		if i := sort.SearchStrings(t.Members, id); i < len(t.Members) && t.Members[i] == id {
			switch t.Level {
			case level.Edit:
				row.HasTeamEditAccess++
			case level.Comment:
				row.HasTeamCommentAccess++
			case level.View:
				row.HasTeamViewAccess++
			}
		}
	}
	if s.Space.UUID != "" && s.Space.Members[id] {
		if s.Space.Kind == "team" {
			row.InTeamSpace = s.Space.UUID
		} else {
			row.InProjectSpace = s.Space.UUID
		}
	}
	return row
}

// DecisionOf is a person's decision on the doc (by uuid).
func (s *Sharing) DecisionOf(userUUID string) DocDecision {
	return DecideDoc(s.rowFor(userUUID), Viewer{UUID: userUUID})
}

// LevelOf is a person's level on the doc now (a deleted doc's is none).
func (s *Sharing) LevelOf(userUUID string) level.Level {
	return s.DecisionOf(userUUID).Level
}

// EveryoneLevel is what someone in the workspace gets with no grant, no team
// and no space: the doc's general access, as it reaches the whole workspace.
func (s *Sharing) EveryoneLevel() level.Level {
	return s.LevelOf(domain.Nobody)
}

// SharingChange is a change to a doc's sharing.
type SharingChange struct {
	// People and Teams set each person's or team's level (by uuid); None takes
	// their grant away. A team's level is at most edit.
	People map[string]level.Level
	Teams  map[string]level.Level
	// General sets the general access; nil keeps it.
	General *GeneralAccess
	// Replace: the doc ends with exactly the people and teams named (its owner
	// keeps owning it), as when a page adopts its new parent's sharing.
	Replace bool
}

// With is the sharing as it would be after the change. Pure.
func (s *Sharing) With(c SharingChange) *Sharing {
	out := *s
	out.People = map[string]level.Level{}
	out.Teams = map[string]*TeamShare{}
	if !c.Replace {
		for k, v := range s.People {
			out.People[k] = v
		}
		for k, v := range s.Teams {
			t := *v
			out.Teams[k] = &t
		}
	}
	owner := ""
	if s.Owner != nil {
		owner = strings.ToLower(s.Owner.Uuid)
	}
	for id, l := range c.People {
		id = strings.ToLower(id)
		switch {
		case id == owner:
		case l == level.None:
			delete(out.People, id)
		default:
			out.People[id] = l
		}
	}
	for id, l := range c.Teams {
		id = strings.ToLower(id)
		if l == level.None {
			delete(out.Teams, id)
			continue
		}
		t := out.Teams[id]
		if t == nil {
			// A team new to the doc: who is in it comes from its own read
			// (TeamMembers); with none known, nobody gains through it here.
			t = &TeamShare{UUID: id}
			if old := s.Teams[id]; old != nil {
				t.Name, t.Members = old.Name, old.Members
			}
		} else {
			t2 := *t
			t = &t2
		}
		t.Level = min(l, level.Edit)
		out.Teams[id] = t
	}
	if c.General != nil {
		out.General = *c.General
	}
	return &out
}

// Shift is a change of level for one person, or for everyone at once.
type Shift struct {
	UserUUID string
	From     level.Level
	To       level.Level
}

// Explained is what a change would do: whose level moves, and what everyone in
// the workspace with no grant of their own gets, before and after.
type Explained struct {
	People   []Shift
	Everyone *Shift
}

// Explain says what going from one sharing to another does: every person
// either one names (by grant, team or space) whose level moves, and the
// workspace's level when it moves. Pure.
func Explain(before, after *Sharing) Explained {
	people := map[string]bool{}
	for _, s := range []*Sharing{before, after} {
		for id := range s.People {
			people[id] = true
		}
		for _, t := range s.Teams {
			for _, m := range t.Members {
				people[m] = true
			}
		}
		for id := range s.Space.Members {
			people[id] = true
		}
	}
	var out Explained
	ids := make([]string, 0, len(people))
	for id := range people {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		from, to := before.LevelOf(id), after.LevelOf(id)
		if from != to {
			out.People = append(out.People, Shift{UserUUID: id, From: from, To: to})
		}
	}
	if from, to := before.EveryoneLevel(), after.EveryoneLevel(); from != to {
		out.Everyone = &Shift{From: from, To: to}
	}
	return out
}

// After is the sharing as it would be after the change, with the teams new to
// the doc read for who is in them. A team that isn't there is
// ErrNoSuchPrincipal.
func (s *Sharing) After(ctx context.Context, c SharingChange) (*Sharing, error) {
	after := s.With(c)
	var unknown []string
	for id, l := range c.Teams {
		if l != level.None && s.Teams[strings.ToLower(id)] == nil {
			unknown = append(unknown, strings.ToLower(id))
		}
	}
	if len(unknown) == 0 {
		return after, nil
	}
	teams, err := domain.TeamMembers(ctx, unknown)
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	for _, t := range teams {
		if t == nil {
			continue
		}
		id := strings.ToLower(t.Uuid)
		ts := after.Teams[id]
		if ts == nil {
			continue
		}
		found[id] = true
		ts.Name, ts.Members = t.Name, domain.PeopleOf(t.Members, t.Admins)
	}
	for _, id := range unknown {
		if !found[id] {
			return nil, ErrNoSuchPrincipal
		}
	}
	return after, nil
}
