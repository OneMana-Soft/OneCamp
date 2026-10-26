package business

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/google/uuid"
)

// A task its source keeps to some of its people (SourceTask.Restricted, such
// as a Jira issue with a security level) comes in restricted, as WA2 keeps a
// task here, so it is never readable more widely than it was. WA2 lets a
// restricted task's project's admins, its assignee and its creator see it,
// and the people granted on it. So each task is settled on its own, a
// sub-task too (a restricted parent makes it restricted, never with the
// parent's people, since its source may let others see it):
//   - whoever ran the import is granted on it;
//   - of the people its source lets see it (SourceTask.Audience, and its
//     assignee), those matched by email take part (PersonMatch.ByEmail): the
//     assignee stays its assignee and the rest are granted, where they are in
//     its project. A name alone never lets anyone into what the source kept
//     to some people;
//   - its creator here is one of those, or else whoever ran the import: no
//     one is let in for having made it.
// Whoever isn't let in is kept in the task's import record and named in the
// report, so the admin can share it on purpose. Who else its source lets see
// it (Jira's groups and roles) isn't known reliably, so the report says to
// share it.
// A comment its source keeps from some of the people who see its task
// (SourceComment.Restricted, such as Jira's kept to a role or a service
// desk's internal note) has no such place on a task here, so it stays in its
// source, and the report counts it on its task (commentsKeptNote).

// sourcePeople are a task's people in its source (source ids): who made it,
// its assignee, and who else it lets see it if it is restricted.
type sourcePeople struct {
	Creator, Assignee string
	Audience          []string
}

// peopleOf is st's people in its source. Pure.
func peopleOf(st *importProvider.SourceTask) sourcePeople {
	p := sourcePeople{Creator: st.CreatedBy, Audience: st.Audience}
	if len(st.AssigneeIds) > 0 {
		p.Assignee = st.AssigneeIds[0]
	}
	return p
}

// leftOut is one of a restricted task's people in its source who isn't let
// in here: as what ("creator", "assignee" or "audience"), and who (their
// source id, and their name here if they came across).
type leftOut struct {
	As     string `json:"as"`
	Source string `json:"source_id"`
	Name   string `json:"-"`
}

// settled is who of a restricted task's people take part here.
type settled struct {
	assignee, creator bool        // they stay its assignee and its creator
	grants            []uuid.UUID // granted, besides whoever ran the import
	left              []leftOut
}

// settle is who of src take part in a restricted task here, given how they
// came across (matched, by source id): see above. Whoever ran the import
// (importer) sees it whatever their part. Pure.
func settle(src sourcePeople, matched map[string]importModels.PersonMatch, importer uuid.UUID) settled {
	in := func(s string) bool { return s != "" && matched[s].ByEmail }
	var out settled
	leave := func(as, s string) {
		if s != "" && (matched[s].ID == uuid.Nil || matched[s].ID != importer) {
			out.left = append(out.left, leftOut{As: as, Source: s, Name: matched[s].Name})
		}
	}
	out.assignee = in(src.Assignee)
	if !out.assignee {
		leave("assignee", src.Assignee)
	}
	for _, s := range src.Audience {
		if in(s) {
			out.grants = append(out.grants, matched[s].ID)
		} else {
			leave("audience", s)
		}
	}
	out.creator = in(src.Creator) && (src.Creator == src.Assignee || slices.Contains(src.Audience, src.Creator))
	if !out.creator {
		leave("creator", src.Creator)
	}
	return out
}

// takePart settles a restricted task's people (in.People, how they came across
// read at once) in its input before the task is written, since its creator
// and assignee are written with it: one who doesn't take part isn't its
// assignee, and whoever ran the import is its creator instead of one. It
// answers the people to grant besides whoever ran the import, and who isn't
// let in.
func takePart(ctx context.Context, job *importModels.Job, in *importTaskInput) ([]uuid.UUID, []leftOut, error) {
	p := in.People
	var asked []string
	for _, s := range append([]string{p.Creator, p.Assignee}, p.Audience...) {
		if s != "" && !slices.Contains(asked, s) {
			asked = append(asked, s)
		}
	}
	matched, err := importModels.PeopleMatched(ctx, job.Id, job.Provider, job.SourceWorkspaceName, asked)
	if err != nil {
		return nil, nil, err
	}
	s := settle(p, matched, in.Importer.UserPostgresInfo.Id)
	if !s.assignee {
		in.Assignee = nil
	}
	if !s.creator {
		in.Creator = in.Importer
	}
	return s.grants, s.left, nil
}

// restrictedParent is why the task a chunk's sub-tasks go under is
// restricted, "" when everyone in its project may see it: they are
// restricted too, each with its own people. Its reason is the one the import
// recorded for it (why), or else that it is restricted here.
func restrictedParent(ctx context.Context, parent uuid.UUID, why string) (string, error) {
	r, err := restrictionModel.Of(ctx, parent)
	if err != nil || !r.Restricted {
		return "", err
	}
	if why == "" {
		return "its parent task is restricted here", nil
	}
	return why, nil
}

// restrictedWhy is why a task comes in restricted, in the report's words,
// from its source's reason ("" for an open task). Pure.
func restrictedWhy(job *importModels.Job, st *importProvider.SourceTask) string {
	if st.Restricted == "" {
		return ""
	}
	return fmt.Sprintf("%s keeps it to some people (%s)", providerName(job.Provider), st.Restricted)
}

// keepRestricted keeps a task being made (taskID, its Postgres row written
// and nothing else yet) to the people who may see it, before its graph node
// and search document exist, so no list, search, notice or export ever shows
// it to anyone else: whoever ran the import and grants (takePart) are
// granted, where they are in its project (projectBusiness.MembersAmong, as
// every grant must be: the graph doesn't know the task yet); its creator is
// let in by the rule.
func keepRestricted(ctx context.Context, job *importModels.Job, in importTaskInput, taskID uuid.UUID, grants []uuid.UUID) error {
	by := uuid.Nil
	if job.TriggeredBy != nil {
		by = *job.TriggeredBy
	}
	seen := map[uuid.UUID]bool{uuid.Nil: true, in.Creator.UserPostgresInfo.Id: true}
	var people []uuid.UUID
	var asked []string
	for _, id := range append([]uuid.UUID{by}, grants...) {
		if !seen[id] {
			seen[id] = true
			people = append(people, id)
			asked = append(asked, id.String())
		}
	}
	inProject, err := projectBusiness.MembersAmong(ctx, in.ProjectUUID.String(), asked)
	if err != nil {
		return err
	}
	var granted []uuid.UUID
	for i, id := range people {
		if inProject[asked[i]] {
			granted = append(granted, id)
		}
	}
	return restrictionModel.Restrict(ctx, taskID, in.ProjectUUID, by, granted, nil, time.Now())
}

// restrictedRecord is what a restricted task's import record keeps of it:
// why, and who of its people in its source isn't let in. Pure.
func restrictedRecord(md map[string]any, why string, left []leftOut) map[string]any {
	md["restricted"] = why
	if len(left) > 0 {
		md["not_let_in"] = left
	}
	return md
}

// restrictedNote is the import's report line saying that a task came in
// restricted, and why, without its name, and who of its people isn't let in,
// so the admin can share it on purpose. A chunk writes its lines at once.
// Pure.
func restrictedNote(job *importModels.Job, entity, sourceID, why string, left []leftOut) importModels.ImportError {
	source := providerName(job.Provider)
	msg := fmt.Sprintf("%s, so it's restricted here: its project's admins, whoever ran the import, and those %s lets see it "+
		"who were matched by email and are in the project see it.", why, source)
	if len(left) > 0 {
		var names []string
		for _, l := range left {
			who := l.Name
			if who == "" {
				who = "someone who didn't come across"
			}
			as := "one " + source + " lets see it"
			switch l.As {
			case "creator":
				as = "who made it"
			case "assignee":
				as = "its assignee"
			}
			names = append(names, who+" ("+as+")")
		}
		msg += " Not let in: " + strings.Join(names, ", ") + "."
	}
	return importModels.ImportError{EntityType: entity, SourceID: sourceID,
		Severity: importModels.SeverityWarning, Code: "TASK_RESTRICTED", Message: msg + " Share it from the task to let others in."}
}

// commentsKeptNote is the import's report line saying how many of a task's
// comments stayed in its source, which keeps them from some of the people who
// see the task (kept is why, for each), without their text, so the admin
// knows they are there: none when there are none. A chunk writes its lines at
// once. Pure.
func commentsKeptNote(job *importModels.Job, taskSourceID string, kept []string) []importModels.ImportError {
	if len(kept) == 0 {
		return nil
	}
	var why []string
	for _, w := range kept {
		if !slices.Contains(why, w) {
			why = append(why, w)
		}
	}
	source := providerName(job.Provider)
	return []importModels.ImportError{{EntityType: importModels.EntityTask, SourceID: taskSourceID,
		Severity: importModels.SeverityWarning, Code: "COMMENTS_RESTRICTED",
		Message: fmt.Sprintf("%s keeps some of the task's comments from people who see it (%s), so they weren't imported and remain in %s. Left there: %d.",
			source, strings.Join(why, "; "), source, len(kept))}}
}
