package business

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	cycleBusiness "github.com/akashc777/OneCamp/business/Cycle"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// A project's cycles from its source (L-2): the cycles that held its tasks
// come over with their names, dates and how they ended, each task goes back
// into its cycle, and the project goes on making cycles as its team did.

// cycleKey is a source cycle's id in the import's map: a source cycle can
// hold the tasks of several projects, and each project has its own copy.
func cycleKey(projectSrc, cycleSrc string) string { return projectSrc + "\x1f" + cycleSrc }

// placeCycles is which of a project's source cycles it gets, in order, and
// which kept cycle each of the rest folds into. A project here runs one cycle
// at a time, and a Linear project's issues can come from several teams whose
// cycles share days: a cycle that shares days with an earlier one folds into
// the one running when it starts, finished tasks and all. Pure.
func placeCycles(cycles []importProvider.SourceCycle) (kept []importProvider.SourceCycle, into map[string]string) {
	sorted := slices.Clone(cycles)
	slices.SortStableFunc(sorted, func(a, b importProvider.SourceCycle) int {
		if c := a.StartsAt.Compare(b.StartsAt); c != 0 {
			return c
		}
		return strings.Compare(a.SourceID, b.SourceID)
	})
	into = map[string]string{}
	for _, c := range sorted {
		// Kept cycles never share days, so the last one ends latest.
		if n := len(kept); n > 0 && kept[n-1].EndsAt.After(c.StartsAt) {
			kept[n-1].Done += c.Done
			into[c.SourceID] = kept[n-1].SourceID
			continue
		}
		kept = append(kept, c)
	}
	return kept, into
}

// makeCycles makes the cycles of a project this import made, in one
// statement, records where each source cycle went, and sets the project
// making cycles as its team did. A cycle closed in the source stays closed,
// with the tasks it finished.
func makeCycles(ctx context.Context, job *importModels.Job, sp *importProvider.SourceProject, project, by uuid.UUID) {
	kept, into := placeCycles(sp.Cycles)
	windows := make([]cycleModel.Window, len(kept))
	for i, c := range kept {
		windows[i] = cycleModel.Window{StartsAt: c.StartsAt, EndsAt: c.EndsAt, Name: truncate(c.Name, 256), CompletedAt: c.CompletedAt}
		if c.CompletedAt != nil {
			windows[i].Done = &kept[i].Done
		}
	}
	made, err := cycleModel.CreateMany(ctx, project, windows, &by)
	if err != nil {
		importModels.LogImportError(ctx, job.Id, nil, importModels.EntityProject, sp.SourceID,
			importModels.SeverityWarning, "CYCLES_NOT_KEPT", "Its cycles couldn't be made: "+err.Error(), nil)
		return
	}
	// Numbered in the order they were given.
	slices.SortFunc(made, func(a, b cycleModel.Cycle) int { return a.Number - b.Number })
	rows := make([]importModels.Mapped, 0, len(sp.Cycles))
	madeFor := make(map[string]uuid.UUID, len(kept))
	for i, c := range kept {
		madeFor[c.SourceID] = made[i].Id
		rows = append(rows, importModels.Mapped{SourceID: cycleKey(sp.SourceID, c.SourceID), OnecampUUID: made[i].Id, CreatedHere: true})
	}
	folded := make([]string, 0, len(into))
	for _, c := range sp.Cycles {
		if to, ok := into[c.SourceID]; ok {
			rows = append(rows, importModels.Mapped{SourceID: cycleKey(sp.SourceID, c.SourceID), OnecampUUID: madeFor[to]})
			folded = append(folded, c.Name+" into "+nameOf(kept, to))
		}
	}
	if err := importModels.RecordMappings(ctx, job.Id, job.Provider, job.SourceWorkspaceName, importModels.EntityCycle, rows); err != nil {
		importModels.LogImportError(ctx, job.Id, nil, importModels.EntityProject, sp.SourceID,
			importModels.SeverityWarning, "CYCLES_NOT_KEPT", "Its tasks couldn't be put back in their cycles: "+err.Error(), nil)
	}
	if len(folded) > 0 {
		importModels.LogImportError(ctx, job.Id, nil, importModels.EntityProject, sp.SourceID,
			importModels.SeverityWarning, "CYCLES_FOLDED", strconv.Itoa(len(folded))+
				" of its cycles shared days with another team's, and a project here runs one cycle at a time, so their tasks are in the cycle running when they started: "+
				strings.Join(folded, "; ")+".", nil)
	}
	keepCycleSchedule(ctx, job, sp, project, by)
}

func nameOf(cycles []importProvider.SourceCycle, id string) string {
	for _, c := range cycles {
		if c.SourceID == id {
			return c.Name
		}
	}
	return ""
}

// keepCycleSchedule sets the project making cycles as its team did in the
// source, after the cycles that came over. One it can't keep (a two-week
// cooldown) is said in the report.
func keepCycleSchedule(ctx context.Context, job *importModels.Job, sp *importProvider.SourceProject, project, by uuid.UUID) {
	s := sp.CycleSchedule
	if s == nil {
		return
	}
	in := cycleBusiness.ScheduleInput{Weeks: s.Weeks, CooldownWeeks: s.CooldownWeeks, Upcoming: s.Upcoming, RollOver: s.RollOver, StartsAt: s.Anchor}
	if _, err := cycleBusiness.SetSchedule(ctx, project, in, by, time.Now()); err != nil {
		importModels.LogImportError(ctx, job.Id, nil, importModels.EntityProject, sp.SourceID,
			importModels.SeverityWarning, "CYCLE_SCHEDULE_NOT_KEPT", "Its cycles don't run themselves here: "+err.Error(), nil)
	}
}

// placeInCycle puts a task in the cycle it was in at the source. A cycle that
// wasn't made here (one its team made after an earlier import made the
// project) is said once in the report.
func placeInCycle(ctx context.Context, job *importModels.Job, st *importProvider.SourceTask, task uuid.UUID) {
	if st.CycleSourceID == "" {
		return
	}
	key := cycleKey(st.ProjectSourceID, st.CycleSourceID)
	cycle, err := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityCycle, key)
	if err == nil && cycle == uuid.Nil {
		cycle, err = importModels.LookupWorkspaceMapping(ctx, job.Provider, job.SourceWorkspaceName, importModels.EntityCycle, key)
	}
	if err == nil && cycle == uuid.Nil {
		warnOnce(ctx, job.Id, nil, importModels.EntityCycle, st.CycleSourceID, "CYCLE_NOT_KEPT",
			"A cycle wasn't made here, so its tasks are in no cycle.", st.ProjectSourceID)
		return
	}
	if err == nil {
		err = cycleModel.SetTask(task, &cycle)
	}
	if err != nil {
		importModels.LogImportError(ctx, job.Id, nil, importModels.EntityTask, st.SourceID,
			importModels.SeverityWarning, "CYCLE_NOT_KEPT", "It couldn't be put back in its cycle: "+err.Error(), nil)
	}
}
