//go:build integration

package business

// L-2: Linear's milestones through the task stage, against Postgres 12 with
// every migration and the graph.
// Run: PKGS=./business/Import/ integ_rel.sh <wt> <log> 'TestImportedMilestones'

import (
	"context"
	"database/sql"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	taskField "github.com/akashc777/OneCamp/business/TaskField"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	workTaxonomy "github.com/akashc777/OneCamp/business/WorkTaxonomy"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/google/uuid"
)

// milestonesProvider streams P1 as Linear's provider does: ENG-3 in the
// Alpha milestone, ENG-4 in none, then Alpha as a Milestone task.
type milestonesProvider struct{ importProvider.Provider }

func (milestonesProvider) DefaultStatusMap() map[string]string   { return map[string]string{} }
func (milestonesProvider) DefaultPriorityMap() map[string]string { return map[string]string{} }
func (milestonesProvider) IterTasksOfProject(ctx context.Context, _ *importModels.Job, _ importProvider.JobOptions, _ string) (<-chan importProvider.SourceTask, <-chan error) {
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	milestone := importProvider.SourceField{SourceID: "project-milestone", Name: "Milestone", Type: importProvider.FieldSelect,
		Options: []importProvider.SourceOption{{SourceID: "ms-a", Label: "Alpha"}}}
	return importProvider.Stream(ctx, "milestones", func() ([]importProvider.SourceTask, error) {
		return []importProvider.SourceTask{
			{SourceID: "i3", SourceKey: "ENG-3", ProjectSourceID: "P1", Name: "Ship the beta", Status: "todo", Created: at,
				Fields: []importProvider.SourceFieldValue{{Field: milestone, Value: "ms-a", Text: "Alpha"}}},
			{SourceID: "i4", SourceKey: "ENG-4", ProjectSourceID: "P1", Name: "Write the notes", Status: "todo", Created: at},
			{SourceID: "milestone:ms-a", ProjectSourceID: "P1", Type: importProvider.TaskMilestone, Name: "Alpha", Created: at,
				DueDay: importProvider.Day{Year: 2026, Month: time.October, Day: 30}},
		}, nil
	})
}

// A Linear milestone becomes a task of the built-in type Milestone, set as
// its row is made, and numbered after every number the project's issues keep
// (ENG-12, a sub-issue, comes in a later chunk). Its issue holds it in the
// project's Milestone field, and a rollback takes the milestone with the
// import's other tasks. Before, milestones were dropped.
func TestImportedMilestones(t *testing.T) {
	ctx := context.Background()
	w := newTaskWorld(t, "linear")
	if _, err := taskKeys.Ensure(ctx, w.project, ""); err != nil {
		t.Fatal(err)
	}
	keepNumbersFree(ctx, &importProvider.SourceProject{SourceID: "P1", HighestNumber: 12}, w.project)
	w.importTasks(t, milestonesProvider{})

	task := func(src string) (id uuid.UUID, number int, kind uuid.NullUUID) {
		t.Helper()
		id, err := importModels.LookupIdMapping(ctx, w.job.Id, importModels.EntityTask, src)
		if err != nil || id == uuid.Nil {
			t.Fatalf("%s wasn't made (%v)", src, err)
		}
		var n sql.NullInt64
		if err := w.env.PG.QueryRow(`SELECT task_number, task_type_id FROM tasks WHERE id = $1`, id).Scan(&n, &kind); err != nil {
			t.Fatal(err)
		}
		return id, int(n.Int64), kind
	}
	issue, issueNumber, issueType := task("i3")
	milestone, milestoneNumber, milestoneType := task("milestone:ms-a")
	if issueNumber != 3 || issueType.Valid {
		t.Errorf("ENG-3 is task 3 of no type, got %d of %v", issueNumber, issueType)
	}
	if milestoneNumber != 13 || !milestoneType.Valid || milestoneType.UUID != workTaxonomy.TypeMilestone {
		t.Errorf("Alpha is task 13, a Milestone, got %d of %v", milestoneNumber, milestoneType)
	}

	fields, err := taskField.List(ctx, w.project)
	if err != nil {
		t.Fatal(err)
	}
	var alpha string
	for _, f := range fields {
		if f.Name == "Milestone" && f.Type == taskField.TypeSelect && len(f.Options) == 1 && f.Options[0].Label == "Alpha" {
			alpha = f.Options[0].ID
			values, err := taskFieldModel.ValuesFor(ctx, []uuid.UUID{issue})
			if err != nil {
				t.Fatal(err)
			}
			if got := compact(values[issue.String()][f.ID.String()]); got != `"`+alpha+`"` {
				t.Errorf("ENG-3's Milestone is %s, want Alpha (%s)", got, alpha)
			}
		}
	}
	if alpha == "" {
		t.Fatalf("the project has no Milestone field with Alpha: %+v", fields)
	}

	if err := importModels.UpdateStatus(ctx, w.job.Id, importModels.StatusCompleted, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := RollbackImport(ctx, w.job.Id); err != nil {
		t.Fatal(err)
	}
	var trashed bool
	if err := w.env.PG.QueryRow(`SELECT deleted_at IS NOT NULL FROM tasks WHERE id = $1`, milestone).Scan(&trashed); err != nil || !trashed {
		t.Fatalf("the rollback leaves the milestone (trashed %v, %v)", trashed, err)
	}
}
