package business

// Dependencies across projects (DG10). One rule for the three places a
// dependency meets the rights of the person acting: making or changing one
// (MayLink), reading one (an end they can't see is dropped whole, never
// counted: VerdictOf's See), and a move that pushes tasks along (only the
// tasks they may change move: VerdictOf's Move). All three ask JudgeTasks,
// so a link they could make is one they can read, and a task the timeline
// shows them moving is one the shift moves.
//
// The rule: in one project, as before, changing the waiting task is enough
// (its dependencies are part of it), so long as the blocker is there for
// them. Across projects, they must be able to change tasks at both ends,
// because a link gives the blocker's project a hold on the other's schedule.
// Through an API token, a member at most (OfMany's asToken), like every other
// decision here.

import (
	"context"
	"errors"
	"strings"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// Verdict is what one person may do with one task, for its dependencies: see
// it, and move it (change its dates, its links).
type Verdict struct {
	See, Move bool
	Standing  Standing
	Facts     TaskFacts
}

// VerdictOf is the verdict for standing s and facts f, by the task rule. Pure.
func VerdictOf(s Standing, f TaskFacts) Verdict {
	return Verdict{See: s.CanOnTask(NeedView, f), Move: s.CanOnTask(NeedEditTask, f), Standing: s, Facts: f}
}

// ErrHiddenEnd is a dependency's other task the person can't see: the caller
// answers as for a task that isn't there, so nothing says it exists.
var ErrHiddenEnd = errors.New("work: the other task isn't there for this person")

// acrossRefusal is ErrAcrossProjects: a Work Access refusal (ErrNotAllowed),
// in words.
type acrossRefusal struct{}

func (acrossRefusal) Error() string {
	return "You can link tasks in two projects only when you can change tasks in both."
}

func (acrossRefusal) Is(target error) bool { return target == ErrNotAllowed }

// ErrAcrossProjects is a dependency across projects by someone who can see
// the other task but not change tasks where it is.
var ErrAcrossProjects error = acrossRefusal{}

// MayLink is nil when the person may make, or change, the dependency of
// waiting on blocker; sameProject says whether both are in one project.
// Otherwise: a Refusal for the waiting task, ErrHiddenEnd for a blocker they
// can't see, ErrAcrossProjects for one in another project they can't change.
// Pure.
func MayLink(waiting, blocker Verdict, sameProject bool) error {
	if err := waiting.Standing.CheckTask(NeedEditTask, waiting.Facts); err != nil {
		return err
	}
	if !blocker.See {
		return ErrHiddenEnd
	}
	if !sameProject && !blocker.Move {
		return ErrAcrossProjects
	}
	return nil
}

// JudgeTasks is the person's verdict on each task (by lower-case uuid), read
// for them (by uuid userUUID and graph uid userUID): each task's project must
// carry the person's member and admin counts, as every project read does. One
// read of their role rows for all the projects, one of the restrictions for
// all the tasks.
func JudgeTasks(ctx context.Context, userUUID, userUID string, ts ...*dgraphStruct.DgraphTask) (map[string]Verdict, error) {
	seen := map[string]bool{}
	var projects []*dgraphStruct.DgraphProject
	for _, t := range ts {
		if t == nil || t.Project == nil || t.Project.Uuid == "" {
			continue
		}
		if key := strings.ToLower(t.Project.Uuid); !seen[key] {
			seen[key] = true
			projects = append(projects, t.Project)
		}
	}
	standings, err := OfMany(ctx, userUUID, projects)
	if err != nil {
		return nil, err
	}
	facts, err := FactsForMany(ctx, userUUID, userUID, ts...)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Verdict, len(ts))
	for _, t := range ts {
		if t == nil {
			continue
		}
		var s Standing
		if t.Project != nil && t.Project.Uuid != "" {
			s = standings[strings.ToLower(t.Project.Uuid)]
		}
		key := strings.ToLower(t.Uuid)
		out[key] = VerdictOf(s, facts[key])
	}
	return out, nil
}

// KeepSeenEnds takes out of task's dependency lists (BlockedBy, Blocks) every
// end in another project the person (by uuid and graph uid) may not see, whole:
// no stub, nothing to count. An end must carry its project as read for the
// person. Ends in task's own project stay as they are (a restricted one the
// filter made a stub stays one, WA2b); a stub from another project goes, its
// project known by its restriction. At most one read of roles and one of
// restrictions, and none when no end is in another project.
func KeepSeenEnds(ctx context.Context, userUUID, userUID string, task *dgraphStruct.DgraphTask) error {
	if task == nil || task.Project == nil {
		return nil
	}
	own := strings.ToLower(task.Project.Uuid)
	var stubs []uuid.UUID
	var others []*dgraphStruct.DgraphTask
	for _, list := range [][]*dgraphStruct.DgraphTask{task.BlockedBy, task.Blocks} {
		for _, e := range list {
			switch {
			case e == nil:
			case e.Hidden:
				if id, err := uuid.Parse(e.Uuid); err == nil {
					stubs = append(stubs, id)
				}
			case e.Project == nil || !strings.EqualFold(e.Project.Uuid, own):
				others = append(others, e)
			}
		}
	}
	if len(stubs) == 0 && len(others) == 0 {
		return nil
	}
	keepStub := map[string]bool{}
	if len(stubs) > 0 {
		rows, err := restrictionsOf(ctx, stubs)
		if err != nil {
			return err
		}
		for id, r := range rows {
			keepStub[id.String()] = strings.EqualFold(r.ProjectID.String(), own)
		}
	}
	var seen map[string]Verdict
	if len(others) > 0 {
		var err error
		if seen, err = JudgeTasks(ctx, userUUID, userUID, others...); err != nil {
			return err
		}
	}
	keep := func(list []*dgraphStruct.DgraphTask) []*dgraphStruct.DgraphTask {
		if list == nil {
			return nil
		}
		out := list[:0]
		for _, e := range list {
			switch {
			case e == nil:
				continue
			case e.Hidden:
				if !keepStub[strings.ToLower(e.Uuid)] {
					continue
				}
			case e.Project == nil:
				continue
			case !strings.EqualFold(e.Project.Uuid, own):
				if !seen[strings.ToLower(e.Uuid)].See {
					continue
				}
			}
			out = append(out, e)
		}
		return out
	}
	task.BlockedBy, task.Blocks = keep(task.BlockedBy), keep(task.Blocks)
	return nil
}
