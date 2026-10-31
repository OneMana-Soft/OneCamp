package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	repairBusiness "github.com/akashc777/OneCamp/business/Repair"
	attachmentDomain "github.com/akashc777/OneCamp/domain/Attachment"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

const (
	// taskFileRepairKey records, once per install, that every file an import
	// kept for a task is linked to the task.
	taskFileRepairKey   = "import_task_files_linked_v1"
	taskFileRepairPage  = 200
	taskFileRepairTries = 12
)

// taskFileRepair is what one pass over the files imports kept for tasks did.
type taskFileRepair struct {
	Linked int // linked to their task
	Listed int // their task lists them already
	NoTask int // their task isn't in the graph
	Failed int // their link failed this pass
}

func (r taskFileRepair) String() string {
	return fmt.Sprintf("%d imported task files linked, %d listed already, %d whose task is gone, %d failed",
		r.Linked, r.Listed, r.NoTask, r.Failed)
}

// TaskFileRepair links every file an import kept for a task to the task
// (repairTaskFiles) once per install. Imports before the file stage linked
// through LinkFile put a task's file under an edge the graph refused, so
// their tasks never listed their files.
var TaskFileRepair = repairBusiness.Once[taskFileRepair]{
	Key: taskFileRepairKey, Name: "import.task-file-repair", Tries: taskFileRepairTries, Repair: repairTaskFiles,
}

// repairTaskFiles links each file imports kept for a task that its task
// doesn't list, a page at a time, with its node rebuilt from the import's
// record and its row, made by the person who kept it. A file its task lists
// already is left as it is: imports since F listed one of its two nodes. A
// file whose link fails is counted and passed, so the rest are linked, and
// the pass then answers an error to run again.
func repairTaskFiles(ctx context.Context) (taskFileRepair, error) {
	var rep taskFileRepair
	for after := uuid.Nil; ; {
		page, err := importModels.KeptTaskFilesAfter(ctx, after, taskFileRepairPage)
		if err != nil {
			return rep, err
		}
		if len(page) == 0 {
			if rep.Failed > 0 {
				return rep, fmt.Errorf("%d imported task files weren't linked", rep.Failed)
			}
			return rep, nil
		}
		after = page[len(page)-1].Id
		recs := make([]*fileRecord, 0, len(page))
		at := make(map[uuid.UUID]importModels.KeptFile, len(page))
		var tasks, files []string
		var keepers []uuid.UUID
		for _, f := range page {
			rec := &fileRecord{ID: f.Id}
			if err := json.Unmarshal(f.Record, rec); err != nil {
				rep.Failed++
				helpers.LogWarnWithContext(ctx, "Imported task file %s: its record: %v", f.Id, err)
				continue
			}
			recs, at[f.Id] = append(recs, rec), f
			tasks, files = append(tasks, rec.ParentUUID), append(files, f.Id.String())
			if f.CreatedBy.Valid {
				keepers = append(keepers, f.CreatedBy.UUID)
			}
		}
		listed, err := attachmentDomain.ListedFiles(ctx, "task", tasks, files)
		if err != nil {
			return rep, err
		}
		makers, err := userDomain.UserUIDsByUUIDs(ctx, keepers)
		if err != nil {
			return rep, err
		}
		for _, rec := range recs {
			if listed[rec.ParentUUID][rec.ID.String()] {
				rep.Listed++
				continue
			}
			f := at[rec.ID]
			switch err := linkFile(ctx, "task", rec.ParentUUID, keptFileNode(rec, f.CreatedAt, makers[f.CreatedBy.UUID])); {
			case err == nil:
				rep.Linked++
			case errors.Is(err, attachmentDomain.ErrNoHolder):
				rep.NoTask++
			default:
				rep.Failed++
				helpers.LogWarnWithContext(ctx, "Imported task file %s wasn't linked: %v", rec.ID, err)
			}
		}
	}
}
