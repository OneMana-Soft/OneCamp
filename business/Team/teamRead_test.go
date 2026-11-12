package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A team is read by a member, a team admin or a workspace admin, and by no
// one else; a team that wasn't found is read by no one, not even a workspace
// admin, so its caller answers as for one that isn't there.
func TestCanReadTeam(t *testing.T) {
	for _, c := range []struct {
		name           string
		team           *dgraphStruct.DgraphTeam
		workspaceAdmin bool
		want           bool
	}{
		{"a member", &dgraphStruct.DgraphTeam{IsMember: 1}, false, true},
		{"a team admin", &dgraphStruct.DgraphTeam{IsAdmin: 1, IsMember: 1}, false, true},
		{"a team admin the member edge missed", &dgraphStruct.DgraphTeam{IsAdmin: 1}, false, true},
		{"a workspace admin outside the team", &dgraphStruct.DgraphTeam{}, true, true},
		{"a workspace admin who is a member", &dgraphStruct.DgraphTeam{IsMember: 1}, true, true},
		{"someone outside the team", &dgraphStruct.DgraphTeam{}, false, false},
		{"no team, for anyone", nil, false, false},
		{"no team, for a workspace admin", nil, true, false},
	} {
		if got := CanReadTeam(c.team, c.workspaceAdmin); got != c.want {
			t.Errorf("%s: CanReadTeam = %v, want %v", c.name, got, c.want)
		}
	}
}
