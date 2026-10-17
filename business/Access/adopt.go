package business

// A page that adopts its new parent's sharing when it moves (A3's move with
// access "adopt"): it ends with exactly the parent's people, teams and general
// access, the parent's owner given full access, and grants that end on the
// parent ending on it at the same moment. Its own owner keeps it. Applied to
// the page and every page under it that the mover has full access to. A new
// subpage takes the same from its parent (CopyOnCreate): one rule for both,
// inheritedFrom.

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	accessModels "github.com/akashc777/OneCamp/models/postgres/Access"
	"github.com/google/uuid"
)

// AdoptChange is the change that gives child its parent's sharing.
func AdoptChange(ctx context.Context, child, parent Resource) (Change, error) {
	if child.Kind != KindDoc || parent.Kind != KindDoc {
		return Change{}, ErrUnknownKind
	}
	p, err := docBusiness.ReadSharing(ctx, parent.UUID)
	if err != nil {
		return Change{}, err
	}
	c, err := docBusiness.ReadSharing(ctx, child.UUID)
	if err != nil {
		return Change{}, err
	}
	ends := map[string]*time.Time{}
	if id, err := uuid.Parse(parent.UUID); err == nil {
		rows, err := accessModels.ListExpiry(ctx, parent.Kind, id)
		if err != nil {
			return Change{}, err
		}
		for _, e := range rows {
			at := e.ExpiresAt
			ends[e.PrincipalKind+":"+strings.ToLower(e.PrincipalUUID.String())] = &at
		}
	}
	owner := ""
	if c.Owner != nil {
		owner = strings.ToLower(c.Owner.Uuid)
	}
	people, teams := inheritedFrom(p, owner)
	general, private := p.General.Level, p.General.Private
	ch := Change{Resource: child, General: &general, Private: &private, Replace: true, ApplyToSubpages: true}
	for _, id := range slices.Sorted(maps.Keys(people)) {
		ch.Add = append(ch.Add, Grant{Kind: PrincipalUser, UUID: id, Level: people[id], Until: ends["user:"+id]})
	}
	for _, id := range slices.Sorted(maps.Keys(teams)) {
		ch.Add = append(ch.Add, Grant{Kind: PrincipalTeam, UUID: id, Level: teams[id], Until: ends["team:"+id]})
	}
	return ch, nil
}

// inheritedFrom is who a page takes from its parent p, for a page owned by
// owner (lower case): the parent's people but the page's own owner, who has
// full by owning it; the parent's owner, at full; and the parent's teams, at
// their levels. A new subpage takes it (CopyOnCreate), and so does a page
// that adopts its new parent's sharing (AdoptChange). Pure.
func inheritedFrom(p *docBusiness.Sharing, owner string) (people, teams map[string]Level) {
	people, teams = map[string]Level{}, map[string]Level{}
	for id, l := range p.People {
		if id != owner {
			people[id] = l
		}
	}
	if p.Owner != nil && p.Owner.Uuid != "" && !strings.EqualFold(p.Owner.Uuid, owner) {
		people[strings.ToLower(p.Owner.Uuid)] = LevelFull
	}
	for id, t := range p.Teams {
		teams[id] = t.Level
	}
	return people, teams
}
