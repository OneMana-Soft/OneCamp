package business

// One manual order per project (PROGRAM.md §16.7): task_rank orders a
// project's tasks, and every view shows that order among the tasks it shows:
// a board column, the list's Manual sort, a section. A drop on the board and
// a drag in the list both come here, so they can't disagree.

import (
	"context"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	taskrank "github.com/akashc777/OneCamp/business/TaskRank"
	domain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// reorderSeams are where Reorder reads, writes and tells: seams for tests.
var reorderSeams = struct {
	neighbours func(ctx context.Context, ids []string) ([]*dgraphStruct.DgraphTask, error)
	project    func(ctx context.Context, projectUUID string) ([]*dgraphStruct.DgraphTask, error)
	write      func(ctx context.Context, ts []*dgraphStruct.DgraphTask) error
	publish    func(m *mqttStruct.MqttTaskChanged)
}{
	neighbours: domain.GetDgraphTaskRanks,
	project:    domain.GetDgraphProjectRanks,
	write:      domain.SetDgraphTaskRanks,
	publish:    mqttBusiness.PublishTaskChanged,
}

// Reorder puts moved (tasks of projectUUID, in the order they keep) between
// the tasks named beforeUUID (above them) and afterUUID (below them); either
// may be empty at an end. The caller has checked the person may change them.
// One read of the neighbours and one write; with no room in the gap the
// project's tasks are renumbered, in the same write.
func Reorder(ctx context.Context, projectUUID string, moved []*dgraphStruct.DgraphTask, beforeUUID, afterUUID string) error {
	if len(moved) == 0 {
		return nil
	}
	isMoved := map[string]bool{}
	for _, m := range moved {
		isMoved[m.Uuid] = true
	}
	var named []string
	for _, id := range []string{beforeUUID, afterUUID} {
		if id != "" && !isMoved[id] {
			named = append(named, id)
		}
	}
	neighbours, err := reorderSeams.neighbours(ctx, named)
	if err != nil {
		return err
	}
	var before, after *dgraphStruct.DgraphTask
	for _, t := range neighbours {
		switch t.Uuid {
		case beforeUUID:
			before = t
		case afterUUID:
			after = t
		}
	}
	writes := make([]*dgraphStruct.DgraphTask, 0, len(moved))
	if ranks, ok := taskrank.BetweenMany(before, after, len(moved)); ok {
		for i, m := range moved {
			r := ranks[i]
			writes = append(writes, &dgraphStruct.DgraphTask{Uuid: m.Uuid, Rank: &r})
		}
	} else {
		all, err := reorderSeams.project(ctx, projectUUID)
		if err != nil {
			return err
		}
		places := make([]*dgraphStruct.DgraphTask, 0, len(moved))
		for _, m := range moved {
			places = append(places, &dgraphStruct.DgraphTask{Uuid: m.Uuid})
		}
		writes = taskrank.RenumberMany(all, places, before, after)
	}
	if err := reorderSeams.write(ctx, writes); err != nil {
		return err
	}
	ranks := make(map[string]float64, len(writes))
	for _, w := range writes {
		ranks[w.Uuid] = *w.Rank
	}
	reorderSeams.publish(&mqttStruct.MqttTaskChanged{TaskUuid: moved[0].Uuid, ProjectUuid: projectUUID, Ranks: ranks})
	return nil
}
