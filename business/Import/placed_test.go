package business

import (
	"slices"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Someone matched by email, or made a placeholder, takes the place the
// source gives them. Someone matched by name alone, or whose first match is
// no longer known, gets none: they don't join a team or a project, and a task
// assigned to them comes in unassigned, saying whom its source assigned it
// to. The report names them (placed.go).
func TestANameAloneGetsNoPlace(t *testing.T) {
	ana, bo, cy, dee := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	matched := map[string]importModels.PersonMatch{
		"u-ana": {ID: ana, Name: "Ana Lima"},
		"u-bo":  {ID: bo, Name: "Bo Ray", ByEmail: true},
		"u-cy":  {ID: cy, Name: "Cy Wu", Placeholder: true},
		"u-dee": {ID: dee, Name: "Dee Moss"},
	}
	in, left := joining([]string{"u-ana", "u-bo", "u-cy", "u-dee", "u-new", "u-bo"}, []string{"u-ana", "u-bo"}, matched)
	if want := []sourceMember{{id: bo, admin: true}, {id: cy}}; !slices.Equal(in, want) {
		t.Errorf("who joins: %+v, want %+v", in, want)
	}
	if want := []string{"Ana Lima", "Dee Moss"}; !slices.Equal(left, want) {
		t.Errorf("who's left out: %v, want %v", left, want)
	}
	if n := membersNote(importModels.EntityProject, "p-launch", left); n.Code != "MEMBERS_NOT_ADDED" || n.SourceID != "p-launch" ||
		!strings.Contains(n.Message, "not added to this project: Ana Lima, Dee Moss.") {
		t.Errorf("the project's report line: %+v", n)
	}
	if n := membersNote(importModels.EntityTeam, "t-launch", left); !strings.Contains(n.Message, "not added to this team: ") {
		t.Errorf("the team's report line: %+v", n)
	}

	person := func(id uuid.UUID, name string) *userModels.UserInfo {
		return &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x" + name[:2], Uuid: id.String(), UserName: name}}
	}
	for _, c := range []struct {
		name     string
		match    importModels.PersonMatch
		info     *userModels.UserInfo
		assignee uuid.UUID
		who      string
	}{
		{"matched by email", matched["u-bo"], person(bo, "Bo Ray"), bo, ""},
		{"a placeholder", matched["u-cy"], person(cy, "Cy Wu"), cy, ""},
		{"matched by name alone", matched["u-ana"], person(ana, "Ana Lima"), uuid.Nil, "Ana Lima"},
		{"first matched by what's no longer known", matched["u-dee"], person(dee, "Dee Moss"), uuid.Nil, "Dee Moss"},
		{"not come across", importModels.PersonMatch{}, nil, uuid.Nil, ""},
	} {
		a, who := assigneeOf(c.match, c.info)
		got := uuid.Nil
		if a != nil {
			got = uuid.MustParse(a.Uuid)
		}
		if got != c.assignee || who != c.who {
			t.Errorf("an assignee %s: assigned to %v, named %q; want %v, %q", c.name, got, who, c.assignee, c.who)
		}
	}

	job := &importModels.Job{Provider: importModels.ProviderLinear}
	if line := unassignedLine(job, "Ana <b>Lima</b>"); line != "<p><em>Imported from Linear: assigned to Ana &lt;b&gt;Lima&lt;/b&gt;, not matched to a member by email.</em></p>" {
		t.Errorf("the task's description says %s", line)
	}
	if line := unassignedLine(job, ""); line != "" {
		t.Errorf("a task its assignee came across for says %s", line)
	}
	if n := unassignedNote(job, nil, importModels.EntityTask, "L-1", "Ana Lima"); n.Code != "ASSIGNEE_NOT_MATCHED" || n.SourceID != "L-1" ||
		!strings.Contains(n.Message, "assigned to Ana Lima in Linear") {
		t.Errorf("the task's report line: %+v", n)
	}
}
