package business

import (
	"context"
	"fmt"
	"html"
	"slices"
	"strings"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A person the source gives a place here (a team's or project's member or
// admin, a task's assignee) takes it only when they were matched by email,
// or are a placeholder an import made for them, whom nobody signs in as
// until invited at their address. A match by name alone attributes their
// work (who made a task, who wrote a comment) but grants them nothing: a
// namesake would join the source's teams and projects, and an assignee reads
// their task wherever it is (Domain.TaskReader.MayRead), finds it in search
// and hears of its comments. Nor does one whose first match is no longer
// known (the import that first matched them was deleted). The report names
// whoever is left out, and an unassigned task's description says whom its
// source assigned it to, for the admin to add or assign them on purpose.

// placed is whether a person who came across as m takes the place the
// source gives them: see above. Pure.
func placed(m importModels.PersonMatch) bool { return m.ByEmail || m.Placeholder }

// sourceMember is one of a team's or project's people who joins it here.
type sourceMember struct {
	id    uuid.UUID
	admin bool
}

// joining is who of a team's or project's members in its source (source
// ids) join it here, as they came across (matched): those placed, each with
// whether the source makes them an admin; and the names of the others who
// came across, who are left out. Someone not mapped yet is neither: a re-run
// picks them up. Pure.
func joining(members, admins []string, matched map[string]importModels.PersonMatch) ([]sourceMember, []string) {
	var in []sourceMember
	var left []string
	seen := map[string]bool{}
	for _, s := range members {
		m, ok := matched[s]
		if !ok || m.ID == uuid.Nil || seen[s] {
			continue
		}
		seen[s] = true
		if placed(m) {
			in = append(in, sourceMember{id: m.ID, admin: slices.Contains(admins, s)})
		} else {
			left = append(left, m.Name)
		}
	}
	return in, left
}

// joiners reads how a team's or project's members came across and answers
// who of them join it (joining), writing the report's line naming those left
// out.
func joiners(ctx context.Context, job *importModels.Job, entity, sourceID string, members, admins []string) ([]sourceMember, error) {
	matched, err := importModels.PeopleMatched(ctx, job.Id, job.Provider, job.SourceWorkspaceName, members)
	if err != nil {
		return nil, err
	}
	in, left := joining(members, admins, matched)
	if len(left) > 0 {
		importModels.LogImportErrors(ctx, job.Id, []importModels.ImportError{membersNote(entity, sourceID, left)})
	}
	return in, nil
}

// membersNote is the import's report line naming who of a team's or
// project's members weren't added to it (joining). Pure.
func membersNote(entity, sourceID string, left []string) importModels.ImportError {
	what := "team"
	if entity == importModels.EntityProject {
		what = "project"
	}
	return importModels.ImportError{EntityType: entity, SourceID: sourceID, Severity: importModels.SeverityWarning, Code: "MEMBERS_NOT_ADDED",
		Message: fmt.Sprintf("Matched to members here by name alone, so not added to this %s: %s. A name doesn't show it's the same person: add them on purpose.",
			what, strings.Join(left, ", "))}
}

// assigneeOf is who a source task's first assignee, as they came across (m,
// info), makes its assignee here: one placed. One matched by name alone
// makes it unassigned, and their name is answered for the task to say whom
// its source assigned it to. Pure.
func assigneeOf(m importModels.PersonMatch, info *userModels.UserInfo) (*dgraphStruct.DgraphUser, string) {
	switch {
	case info == nil:
		return nil, ""
	case placed(m):
		a := info.UserDgraphInfo
		return &a, ""
	}
	return nil, info.UserDgraphInfo.DisplayName()
}

// ofTask is who a source task's people are here: who made it (whoever ran
// the import when its maker didn't come across), its assignee (assigneeOf),
// the name of an assignee matched by name alone, whom it isn't assigned to,
// and the names of its other assignees.
func (p *people) ofTask(ctx context.Context, st *importProvider.SourceTask, importer *userModels.UserInfo) (creator *userModels.UserInfo, assignee *dgraphStruct.DgraphUser, unplaced string, coAssignees []string) {
	creator, coAssignees = importer, []string{}
	if st.CreatedBy != "" {
		if _, info := p.resolve(ctx, st.CreatedBy); info != nil {
			creator = info
		}
	}
	if len(st.AssigneeIds) > 0 {
		assignee, unplaced = assigneeOf(p.resolve(ctx, st.AssigneeIds[0]))
		coAssignees = coAssigneeNames(ctx, p, st.AssigneeIds[1:])
	}
	return creator, assignee, unplaced, coAssignees
}

// unassignedLine is what a task's description says first when its source
// assignee (who) was matched by name alone (assigneeOf), and nothing when
// who is "". Pure.
func unassignedLine(job *importModels.Job, who string) string {
	if who == "" {
		return ""
	}
	return "<p><em>Imported from " + html.EscapeString(providerName(job.Provider)) + ": assigned to " + html.EscapeString(who) +
		", not matched to a member by email.</em></p>"
}

// unassignedNote is the import's report line saying that a task came in
// unassigned, as its source assignee (who) was matched by name alone. A
// chunk writes its lines at once. Pure.
func unassignedNote(job *importModels.Job, chunkID *uuid.UUID, entity, sourceID, who string) importModels.ImportError {
	return importModels.ImportError{ChunkID: chunkID, EntityType: entity, SourceID: sourceID, Severity: importModels.SeverityWarning, Code: "ASSIGNEE_NOT_MATCHED",
		Message: fmt.Sprintf("It was assigned to %s in %s, matched to a member here by name alone, so it's unassigned. A name doesn't show it's the same person: assign it on purpose.",
			who, providerName(job.Provider))}
}
