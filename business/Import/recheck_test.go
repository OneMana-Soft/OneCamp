package business

import (
	"encoding/json"
	"maps"
	"reflect"
	"testing"

	"github.com/google/uuid"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
)

// What the re-check does to a task, by how its source has it now and how it
// is here: the fixed importer's rule for one the source keeps to some
// people, nothing for one someone restricted here, or opened after a re-check
// kept it, or the source no longer answers for, and an admin's containment
// lifted for one the source shows to everyone.
func TestTheRecheckPlansEachTaskByItsSourceAndHowItIsHere(t *testing.T) {
	pip, rae, ada := uuid.New(), uuid.New(), uuid.New()
	kept := importProvider.RecheckedTask{Task: importProvider.SourceTask{Restricted: "an issue security level",
		Audience: []string{"u-rae"}, AssigneeIds: []string{"u-pip"}, CreatedBy: "u-cy"}}
	open := importProvider.RecheckedTask{Task: importProvider.SourceTask{AssigneeIds: []string{"u-pip"}, CreatedBy: "u-cy"}}
	gone := importProvider.RecheckedTask{Gone: true}
	// Rae is let in; Pip, the assignee, and Cy, its maker, aren't.
	outside := settled{grants: []uuid.UUID{rae}}
	inside := settled{assignee: true, creator: true, grants: []uuid.UUID{rae}}
	restricted := func(unassign, creatorOutside, reassigned bool) taskPlan {
		return taskPlan{outcome: outcomeRestricted, grants: []uuid.UUID{rae}, unassign: unassign, creatorOutside: creatorOutside, reassigned: reassigned}
	}
	for _, c := range []struct {
		name string
		now  importProvider.RecheckedTask
		here taskHere
		s    settled
		want taskPlan
	}{
		{"kept at the source: kept to whom it lets in, its assignee off, its maker counted", kept, taskHere{assignee: pip}, outside, restricted(true, true, false)},
		{"kept, with its assignee and maker let in", kept, taskHere{assignee: pip}, inside, restricted(false, false, false)},
		{"kept, and given to someone else here since", kept, taskHere{assignee: ada}, outside, restricted(false, true, true)},
		{"kept, with no assignee here", kept, taskHere{}, outside, restricted(false, true, false)},
		{"kept, restricted here by this re-check before", kept, taskHere{restricted: true, ours: true, assignee: pip}, outside, restricted(true, true, false)},
		{"kept, and an admin's containment keeps it", kept, taskHere{restricted: true, ours: true, contained: true}, outside, restricted(false, true, false)},
		{"kept, but someone restricted it here", kept, taskHere{restricted: true, assignee: pip}, outside, taskPlan{outcome: outcomeAlready}},
		{"kept, but someone opened it after this re-check kept it", kept, taskHere{ours: true, openedSince: true, assignee: pip}, outside, taskPlan{outcome: outcomeOpenedSince}},
		{"open at the source", open, taskHere{assignee: pip}, outside, taskPlan{outcome: outcomeOpen}},
		{"open at the source, and an admin's containment keeps it", open, taskHere{restricted: true, ours: true, contained: true}, outside, taskPlan{outcome: outcomeOpened}},
		{"open at the source, and someone opened an admin's containment since", open, taskHere{ours: true, contained: true, openedSince: true}, outside, taskPlan{outcome: outcomeOpenedSince}},
		{"open at the source, but someone restricted it here", open, taskHere{restricted: true}, outside, taskPlan{outcome: outcomeAlready}},
		{"gone from the source", gone, taskHere{assignee: pip}, outside, taskPlan{outcome: outcomeGone}},
		{"gone from the source, and an admin's containment keeps it", gone, taskHere{restricted: true, ours: true, contained: true}, outside, taskPlan{outcome: outcomeGone}},
	} {
		if got := planTask(c.now, c.here, c.s, pip); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v; want %+v", c.name, got, c.want)
		}
	}
}

// A connection proves it reaches the import's own site when most of the
// import's sampled tasks answer, each made by the person the import recorded
// (a maker never changes at the source), and at least half of those still in
// the project the import recorded (tasks move between projects). A
// connection to another site is never used, and one answer alone proves
// nothing.
func TestAConnectionMustReachTheImportsOwnSite(t *testing.T) {
	sample := []importModels.MappedTask{
		{SourceID: "HR-1", ProjectSource: "HR", Author: "u-cy"},
		{SourceID: "HR-2", ProjectSource: "HR"}, // its record names no maker
		{SourceID: "HR-3", ProjectSource: "HR", Author: "u-rae"},
		{SourceID: "HR-4", ProjectSource: "HR", Author: "u-cy"},
	}
	as := func(project, maker string) importProvider.RecheckedTask {
		return importProvider.RecheckedTask{Task: importProvider.SourceTask{ProjectSourceID: project, CreatedBy: maker}}
	}
	gone := importProvider.RecheckedTask{Gone: true}
	// The site as the import recorded it, but for what over changes.
	recorded := func(over map[string]importProvider.RecheckedTask) map[string]importProvider.RecheckedTask {
		out := map[string]importProvider.RecheckedTask{"HR-1": as("HR", "u-cy"), "HR-2": as("HR", "u-al"), "HR-3": as("HR", "u-rae"), "HR-4": as("HR", "u-cy")}
		maps.Copy(out, over)
		return out
	}
	for _, c := range []struct {
		name string
		got  map[string]importProvider.RecheckedTask
		same bool
	}{
		{"each answers as recorded", recorded(nil), true},
		{"one has moved to another project since", recorded(map[string]importProvider.RecheckedTask{"HR-3": as("OPS", "u-rae")}), true},
		{"half have moved", recorded(map[string]importProvider.RecheckedTask{"HR-1": as("OPS", "u-cy"), "HR-3": as("FIN", "u-rae")}), true},
		{"one is gone", recorded(map[string]importProvider.RecheckedTask{"HR-4": gone}), true},
		{"most answer in other projects", recorded(map[string]importProvider.RecheckedTask{"HR-1": as("OPS", "u-cy"), "HR-3": as("OPS", "u-rae"), "HR-4": as("FIN", "u-cy")}), false},
		{"one answers made by someone else: another site's", recorded(map[string]importProvider.RecheckedTask{"HR-3": as("HR", "u-zed")}), false},
		{"half are gone", recorded(map[string]importProvider.RecheckedTask{"HR-1": gone, "HR-2": gone}), false},
		{"one answers alone", map[string]importProvider.RecheckedTask{"HR-1": as("HR", "u-cy")}, false},
		{"none answers", map[string]importProvider.RecheckedTask{"HR-1": gone, "HR-2": gone, "HR-3": gone, "HR-4": gone}, false},
		{"nothing comes back", nil, false},
	} {
		if got := answersAsRecorded(sample, c.got); got != c.same {
			t.Errorf("%s: same site %v, want %v", c.name, got, c.same)
		}
	}
}

// Whose a task's recorded restriction is when a pass reads it again: the
// re-check's while the task is as it left it, or while the change is still to
// be made on a task that is open or restricted on its own (a pass that
// stopped between writing the new people and restricting the task leaves it
// on the people before); someone's once they restricted it otherwise; and
// opened since once someone opened a task it kept.
func TestARecheckFinishesItsOwnChangeAndLeavesSomeonesChoice(t *testing.T) {
	ada, ben, parent := uuid.New(), uuid.New(), uuid.New()
	change := func(pending bool, people ...uuid.UUID) *reviewModel.AccessChange {
		after := map[string]any{"people": people}
		if pending {
			after["pending"] = true
		}
		raw, err := json.Marshal(after)
		if err != nil {
			t.Fatal(err)
		}
		return &reviewModel.AccessChange{Kind: reviewModel.ChangeRestricted, After: raw}
	}
	open := restrictionModel.Restriction{}
	toAda := restrictionModel.Restriction{Restricted: true, Grants: []uuid.UUID{ada}}
	viaParent := restrictionModel.Restriction{Restricted: true, Grants: []uuid.UUID{ada}, Via: parent}
	for _, c := range []struct {
		name          string
		here          restrictionModel.Restriction
		ours          *reviewModel.AccessChange
		still, opened bool
	}{
		{"no change recorded", toAda, nil, false, false},
		{"kept as it left it", toAda, change(false, ada), true, false},
		{"new people written, not made yet", toAda, change(true, ben), true, false},
		{"not made yet, the task open", open, change(true, ada), true, false},
		{"not made yet, kept through its parent", viaParent, change(true, ben), false, false},
		{"restricted to others since", toAda, change(false, ben), false, false},
		{"opened since", open, change(false, ada), true, true},
	} {
		got, opened := ourChange(c.here, c.ours)
		if (got != nil) != c.still || opened != c.opened {
			t.Errorf("%s: ours %v, opened since %v; want %v, %v", c.name, got != nil, opened, c.still, c.opened)
		}
		if kept := keptByUs(c.here, c.ours); kept != (c.still && c.here.Restricted) {
			t.Errorf("%s: kept by the re-check %v, want %v", c.name, kept, !kept)
		}
	}
}
