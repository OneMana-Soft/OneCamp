package businness

// A doc's space, as who may open the doc goes: everyone in a project's or a
// team's space reaches every doc in it (docAccessDecide.go). So a doc moved
// into a space is shared with its people, and one moved out is taken from
// them; business/Access decides who may do either, and explains it.

import (
	"context"
	"maps"
	"strings"

	domain "github.com/akashc777/OneCamp/domain/Doc"
)

// ErrNoSuchSpace is a space that isn't there: no live project or team with
// that uuid, or a kind of space that isn't one.
var ErrNoSuchSpace = domain.ErrNoSuchSpace

// Space is a project's or a team's space, as the docs in it are shared. The
// zero Space is no space.
type Space struct {
	Kind string // "project" or "team"
	UUID string
	Name string
	// Members are the uuids of its members and admins, in lower case.
	Members map[string]bool
}

// Ref is the space as an event names it: "project:<uuid>" or "team:<uuid>",
// and "none" for no space.
func (sp Space) Ref() string {
	if sp.Kind == "" || sp.UUID == "" {
		return "none"
	}
	return sp.Kind + ":" + sp.UUID
}

// SpaceChange is a move's change of a doc's space, as its doc.moved tells
// it (EmitMoved): the space it went into and the one it left (the zero Space
// for none), and its owner, who is told when someone else moved it.
type SpaceChange struct {
	To, From Space
	Owner    string
}

// ReadSpace reads a live project's or team's space, with who is in it.
// ErrNoSuchSpace when there's none.
func ReadSpace(ctx context.Context, kind, spaceUUID string) (Space, error) {
	name, people, err := domain.GetSpace(ctx, kind, spaceUUID)
	if err != nil {
		return Space{}, err
	}
	return spaceNamed(kind, spaceUUID, name, people), nil
}

// spaceNamed is a space as a read finds it: its kind and uuid, its name, and
// who is in it (domain.PeopleOf). The zero Space for no kind or no uuid. Pure.
func spaceNamed(kind, spaceUUID, name string, people []string) Space {
	if kind == "" || spaceUUID == "" {
		return Space{}
	}
	sp := Space{Kind: kind, UUID: spaceUUID, Name: name, Members: make(map[string]bool, len(people))}
	for _, id := range people {
		sp.Members[strings.ToLower(id)] = true
	}
	return sp
}

// InSpace is the sharing as it would be with the doc in sp instead, its
// people, teams and general access as they are. Pure.
func (s *Sharing) InSpace(sp Space) *Sharing {
	out := *s
	out.Space = sp
	out.Space.Members = maps.Clone(sp.Members)
	return &out
}
