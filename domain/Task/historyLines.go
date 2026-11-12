package Domain

import (
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// historyLine is one history line by the person (graph uid by), as a write's
// JSON: its kind and the states before and after ("" writes none). Pure but
// for its new uuid.
func historyLine(by, kind, prev, next string, at time.Time) map[string]any {
	l := map[string]any{"dgraph.type": "Activity", "activity_uuid": uuid.NewString(), "activity_type": kind,
		"activity_by": map[string]any{"uid": by}, "activity_time": at}
	if prev != "" {
		l["activity_prev_state"] = prev
	}
	if next != "" {
		l["activity_next_state"] = next
	}
	return l
}

// onTask is the write that adds line to the history of the task with graph
// uid uid.
func onTask(uid string, line map[string]any) map[string]any {
	return map[string]any{"uid": uid, "task_activities": []map[string]any{line}}
}

// moveHistory is the history a move (ChangeParent) writes in its one
// mutation, by the person (graph uid by): the task names the task it sat
// under and the one it goes under (ACTIVITY_TYPE_PARENT; "" is the top); the
// one it left names it by uuid (ACTIVITY_TYPE_REMOVE_SUB_TASK: no longer its
// sub-task, it is judged by its id); the one it goes under names it by
// childName (ACTIVITY_TYPE_ADD_SUB_TASK, as making a sub-task writes). Pure
// but for the lines' uuids.
func moveHistory(by string, child Place, childName string, parent *Place, at time.Time) []map[string]any {
	next := ""
	if parent != nil {
		next = parent.UUID
	}
	out := []map[string]any{onTask(child.Uid, historyLine(by, dgraphStruct.ACTIVITY_TYPE_PARENT, child.Parent, next, at))}
	if child.ParentUid != "" {
		out = append(out, onTask(child.ParentUid, historyLine(by, dgraphStruct.ACTIVITY_TYPE_REMOVE_SUB_TASK, "", child.UUID, at)))
	}
	if parent != nil {
		out = append(out, onTask(parent.Uid, historyLine(by, dgraphStruct.ACTIVITY_TYPE_ADD_SUB_TASK, "", childName, at)))
	}
	return out
}

// linkHistory is the history a link change (ChangeLink) of kind between from
// and to writes in its one mutation, by the person (graph uid by): a line on
// each task naming the other by uuid, with the link as that task reads it
// (LinkLabel). Pure but for the lines' uuids.
func linkHistory(by, kind string, remove bool, from, to Place, at time.Time) []map[string]any {
	line := dgraphStruct.ACTIVITY_TYPE_ADD_LINK
	if remove {
		line = dgraphStruct.ACTIVITY_TYPE_REMOVE_LINK
	}
	return []map[string]any{
		onTask(from.Uid, historyLine(by, line, LinkLabel(kind, "out"), to.UUID, at)),
		onTask(to.Uid, historyLine(by, line, LinkLabel(kind, "in"), from.UUID, at)),
	}
}
