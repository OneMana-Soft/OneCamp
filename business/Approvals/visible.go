package business

// Approvers who can see the item (PROJ-LEAD-4's review of R2b): being in the
// project isn't enough for a task kept to some members (WA2), so each one is
// checked by the one rule for who reads a task, its restriction included.
// One who can't see it is never asked: a notice about a task they can't open.

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Seam for tests: which of the people may read the task now, each as
// themselves (the task the gate holds was read for whoever moved it).
var readersOf = taskDomain.Readers

// seeing splits approvers into those who can see the task and the names of
// those who can't. Someone who has left isn't asked or named; a read that
// fails is its error, so a passing fault never drops an approver who is
// still here.
func seeing(ctx context.Context, task *dgraphStruct.DgraphTask, ids []string) (kept []string, cant []string, err error) {
	readers, err := readersOf(ctx, task.Uuid, ids)
	if err != nil {
		return nil, nil, err
	}
	for _, raw := range ids {
		id, perr := uuid.Parse(raw)
		if perr != nil {
			continue
		}
		who, aerr := actorOf(ctx, id)
		if errors.Is(aerr, userDomain.ErrNoActor) {
			continue
		}
		if aerr != nil {
			return nil, nil, aerr
		}
		if !readers[raw] {
			cant = append(cant, who.UserDgraphInfo.DisplayName())
			continue
		}
		kept = append(kept, raw)
	}
	return kept, cant, nil
}

// seeingNow is the recipients of a notice about a request on a task who may
// read the task now (seeing). A request's people were seen when it was asked,
// but its answer and its reminders come later, after someone may have been
// taken off the project or kept out by a restriction: a notice names the task.
// When that can't be read, nobody is told.
func seeingNow(ctx context.Context, taskUUID string, to []notificationBusiness.NoticeRecipient) []notificationBusiness.NoticeRecipient {
	ids := make([]string, len(to))
	for i, r := range to {
		ids[i] = r.UserUUID
	}
	kept, _, err := seeing(ctx, &dgraphStruct.DgraphTask{Uuid: taskUUID}, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Approvals: who may read task %s couldn't be read, so nobody is told: %v", taskUUID, err)
		return nil
	}
	var out []notificationBusiness.NoticeRecipient
	for _, r := range to {
		if slices.Contains(kept, r.UserUUID) {
			out = append(out, r)
		}
	}
	return out
}
