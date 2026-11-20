package business

// The AI search index's copy of every task's assignee, put right.
//
// A task's AI entry names its assignee, and Ask AI finds the task for them by
// that alone from outside its project. Hand-overs are written onto the entry
// (assigneeAI.go), but an entry can still name someone the task has gone
// from: a hand-over made before they were written, or one whose write and its
// retry both failed. Every search checks that grant against the task as it is
// (services/AI keepAssigned), so such an entry gives the old assignee
// nothing; but the assignee now, from outside the project, misses the task.
// This writes every task's assignee now onto its entry, a page of tasks in one
// write. An entry already right is left alone, so what it reports is how many
// entries were wrong.
//
// Trashed tasks are written too: a trashed task's entry stays (every search
// leaves it out), and a restore brings it back as it is, so it must name the
// assignee now, as a hand-over in the trash writes it (AssignInTrash).
//
// It runs in the background at startup, where AI is on, until it has
// succeeded once, which the system_configs key below records, as the docs'
// privacy repair does (business/Doc embeddingAccessBackfill.go). AI only:
// without-ai drops this file, and StartAIAssigneeBackfill starts nothing
// there.

import (
	"context"
	"errors"
	"fmt"

	repairBusiness "github.com/akashc777/OneCamp/business/Repair"
	domain "github.com/akashc777/OneCamp/domain/Task"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	aiAssigneeBackfillKey  = "ai_embeddings_task_assignee_v1"
	aiAssigneeBackfillPage = 200
	// aiAssigneeBackfillTries bounds the retries in one process: about three
	// and a half hours of waits, then the next start tries again.
	aiAssigneeBackfillTries = 12
)

// AIAssigneeCheck is what one pass did: the tasks whose assignee it wrote
// onto their AI search entries, and the entries that said something else and
// were corrected.
type AIAssigneeCheck struct{ Tasks, Corrected int }

func (c AIAssigneeCheck) String() string {
	return fmt.Sprintf("checked the assignee of %d tasks on their AI search entries and corrected %d entries", c.Tasks, c.Corrected)
}

// AIAssigneeBackfill writes every task's assignee now onto its AI search
// entry (backfillAIAssignees) once per install.
var AIAssigneeBackfill = repairBusiness.Once[AIAssigneeCheck]{
	Key: aiAssigneeBackfillKey, Name: "task.ai-assignee-backfill", Tries: aiAssigneeBackfillTries, Repair: backfillAIAssignees,
}

// It starts only where AI is on (aiOn: the server starts its loops after
// InitAIService). Where AI was never turned on there is no AI search index,
// so every try would fail and nothing would be recorded; where it is off,
// nothing reads the entries. AI turned on while the server runs is repaired
// at the next start: until then an entry left naming a former assignee is a
// miss, never a leak (V-1123).
func init() {
	startAIAssigneeBackfill = func(ctx context.Context) {
		if aiOn() {
			startAIAssigneeOnce(ctx)
		}
	}
}

// Seams for tests.
var (
	assigneePage = domain.TaskAssigneePage
	aiReindexing = ai.IsReindexRunning
	// startAIAssigneeOnce starts the repair (AIAssigneeBackfill.Start).
	startAIAssigneeOnce = func(ctx context.Context) { AIAssigneeBackfill.Start(ctx) }
)

// backfillAIAssignees writes every task's assignee now onto its AI search
// entry: a page of tasks read from the graph, then written in one
// update-by-query (setAIAssignees, the hand-overs' writer).
func backfillAIAssignees(ctx context.Context) (c AIAssigneeCheck, err error) {
	if aiReindexing() {
		return c, errors.New("the AI search index is being rebuilt")
	}
	for offset := 0; ; offset += aiAssigneeBackfillPage {
		page, err := assigneePage(ctx, offset, aiAssigneeBackfillPage)
		if err != nil {
			return c, err
		}
		to := make(map[string]string, len(page))
		for _, p := range page {
			to[p.Task] = p.Assignee
		}
		n, err := setAIAssignees(ctx, to)
		c.Corrected += n
		if err != nil {
			return c, err
		}
		c.Tasks += len(page)
		if len(page) < aiAssigneeBackfillPage {
			break
		}
	}
	// A rebuild that began meanwhile copied the index before these writes
	// reached it, so they are done again once it has finished.
	if aiReindexing() {
		return c, errors.New("the AI search index is being rebuilt")
	}
	return c, nil
}
