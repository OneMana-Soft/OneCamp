package business

// A team as the Access Resolver decides it, so what belongs to a team (its
// calendar, gcal-14) is as the team is, by the one rule its own pages ask
// (CanReadTeam): its members and its admins, and nobody else. A workspace
// admin gets nothing more here than anywhere through the resolver (its
// Subject.Admin). A team admin runs it (full); a member works in it (edit);
// an archived team is read alone.

import (
	"context"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkDomain "github.com/akashc777/OneCamp/domain/EntityLink"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// KindTeam is a team, by its uuid, as the Access Resolver names it.
const KindTeam = "team"

func init() { accessBusiness.RegisterKind(KindTeam, teamRules{}) }

// readTeamStandings reads teams with whether one reader is a member or an
// admin of each; tests swap it.
var readTeamStandings = entityLinkDomain.TeamChips

type teamRules struct{}

// DecideMany is every team asked, in one read.
func (teamRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	if who.UserUID == "" || len(ids) == 0 {
		return out, nil
	}
	asked := map[string][]string{}
	for _, id := range ids {
		key := strings.ToLower(strings.TrimSpace(id))
		asked[key] = append(asked[key], id)
	}
	teams, err := readTeamStandings(ctx, ids, who.UserUID)
	if err != nil {
		return nil, err
	}
	for _, tm := range teams {
		d := decideTeam(tm.Member > 0, tm.Admin > 0, helpers.IsSoftDeleted(tm.DeletedAt))
		for _, id := range asked[strings.ToLower(tm.UUID)] {
			out[id] = d
		}
	}
	return out, nil
}

// decideTeam is a team's rule for one person. Pure.
func decideTeam(member, admin, archived bool) accessBusiness.Decision {
	var d accessBusiness.Decision
	if !CanReadTeam(&dgraphStruct.DgraphTeam{IsMember: boolCount(member), IsAdmin: boolCount(admin)}, false) {
		return d
	}
	d.Level, d.Reasons = accessBusiness.LevelEdit, []string{"member"}
	if admin {
		d.Level, d.Reasons = accessBusiness.LevelFull, []string{"admin"}
	}
	if archived {
		d.Level, d.Archived = accessBusiness.LevelView, true
	}
	d.Was = d.Level
	return d
}

func boolCount(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}
