package Domain

// The work hierarchy (jira-01 J-H): the one parent/sub-task relation
// (task_parent_task, task_sub_tasks), three deep. An epic (the built-in type,
// business/WorkTaxonomy TypeEpic, kept in Postgres) sits at the top and holds
// tasks; a task holds sub-tasks; a sub-task holds nothing. A task and the task
// it sits under are in one project. An epic's tasks are top-level work: the
// backlog, the timeline and the plan read them as they read a task at the top
// (TopLevel), and they take dependencies; a sub-task does neither.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskKeyModel "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
)

// ErrHierarchy is a place in the hierarchy a task can't take; the refusal's
// words say why.
var ErrHierarchy = errors.New("tasks: that place in the hierarchy isn't open")

// refusal is a refusal of kind (ErrHierarchy, ErrLink) in words.
type refusal struct {
	words string
	kind  error
}

func (r refusal) Error() string        { return r.words }
func (r refusal) Is(target error) bool { return target == r.kind }

func refuse(words string) error { return refusal{words, ErrHierarchy} }

// Refusal is a hierarchy refusal (ErrHierarchy) in words.
func Refusal(words string) error { return refuse(words) }

// MaxEpics is how many epics a project holds: the top-level reads name them
// all (TopLevel), so the number stays bounded.
const MaxEpics = 500

// Place is what the hierarchy's rules read of one task.
type Place struct {
	UUID    string
	Uid     string
	Project string
	// Epic is whether its type is Epic.
	Epic bool
	// Parent is the task it sits under ("" at the top), ParentUid that
	// task's graph uid, and ParentEpic whether that one is an epic.
	Parent     string
	ParentUid  string
	ParentEpic bool
	// SubTasks is how many live tasks sit under it.
	SubTasks int
	// Deep is whether a task under it holds tasks of its own.
	Deep bool
	// Dependent is whether it waits on a task or one waits on it;
	// KidsDependent, whether a task under it does.
	Dependent, KidsDependent bool
}

// Level is where it sits: 0 an epic, 1 a task at the top or in an epic, 2 a
// sub-task.
func (p Place) Level() int {
	switch {
	case p.Epic:
		return 0
	case p.Parent == "" || p.ParentEpic:
		return 1
	}
	return 2
}

// SubTask is whether it is a sub-task: under a task that isn't an epic.
func (p Place) SubTask() bool { return p.Level() == 2 }

// MayParent is nil when child may sit under parent (nil: at the top). Pure.
func MayParent(child Place, parent *Place) error {
	if parent == nil {
		return nil
	}
	switch {
	case child.Epic:
		return refuse("An epic sits at the top: it can't go under another task.")
	case parent.UUID == child.UUID:
		return refuse("A task can't go under itself.")
	case parent.Project != child.Project:
		return refuse("A task goes under a task of its own project.")
	case parent.Level() == 2:
		return refuse("A sub-task holds no tasks: put it under an epic or a task at the top.")
	case parent.Epic && child.Deep:
		return refuse("That task's sub-tasks hold tasks of their own, which an epic's tasks can't.")
	case !parent.Epic && child.SubTasks > 0:
		return refuse("A task with sub-tasks can't become a sub-task: put it under an epic, or take its sub-tasks out first.")
	case !parent.Epic && child.Dependent:
		return refuse("A task with dependencies can't become a sub-task, because sub-tasks take none: remove its dependencies first.")
	}
	return nil
}

// MayType is nil when p may become an epic (epic true) or stop being one.
// Pure.
func MayType(p Place, epic bool) error {
	switch {
	case epic && !p.Epic && p.Parent != "":
		return refuse("An epic sits at the top: take the task out from under its parent first.")
	case !epic && p.Epic && p.Deep:
		return refuse("This epic's tasks have sub-tasks, so it stays an epic: their sub-tasks would sit too deep.")
	case !epic && p.Epic && p.KidsDependent:
		return refuse("This epic's tasks have dependencies, so it stays an epic: as sub-tasks they could take none.")
	}
	return nil
}

// topLevel is the clause for top-level work, given the epics a read may
// meet, and the block it needs: a task at the top, or one in an epic. work
// leaves the epics themselves out as well: an epic is a container, so a total
// of work (the plan's) counts its tasks and never its own estimate or dates,
// while a list (the backlog, the timeline) still shows it. No epics: today's
// clause and no block. Pure.
func topLevel(epics []string, work bool) (block, clause string) {
	list := dgraphquery.UUIDList(epics)
	if list == "" {
		return "", "NOT has(task_parent_task)"
	}
	block, clause = "var(func: eq(task_uuid, ["+list+"])) { inEpic as task_sub_tasks }", "(NOT has(task_parent_task) OR uid(inEpic))"
	if work {
		// Bound only here: Dgraph refuses a variable a query never uses.
		block, clause = "ep as "+block, clause+" AND NOT uid(ep)"
	}
	return block, clause
}

// EpicsIn is the ids of the epics of projects, read from Postgres (the type
// lives there, and nowhere else): at most MaxEpics a project.
func EpicsIn(ctx context.Context, projects ...string) ([]string, error) {
	ids := make([]uuid.UUID, 0, len(projects))
	for _, p := range projects {
		if id, perr := uuid.Parse(p); perr == nil {
			ids = append(ids, id)
		}
	}
	return taskKeyModel.Epics(ctx, ids)
}

// TopLevel is topLevel for the epics of projects (EpicsIn): what a list of
// them shows at the top, epics included.
func TopLevel(ctx context.Context, projects ...string) (block, clause string, err error) {
	return topLevelOf(ctx, false, projects)
}

// TopLevelWork is TopLevel without the epics themselves: what a total of
// work counts (domain PlanTasksOf, ReadPlanTotalsOf).
func TopLevelWork(ctx context.Context, projects ...string) (block, clause string, err error) {
	return topLevelOf(ctx, true, projects)
}

// DropEpics takes the epics of projects (EpicsIn) out of each list of tasks
// read: TopLevelWork's rule (an epic is a container, so a total of work counts
// its tasks and never its own estimate or dates) for a read that can't name
// its projects before it reads (the people workload, across the reader's
// projects). One read of the epics, none when no list holds a task.
func DropEpics(ctx context.Context, projects []string, lists ...*[]*dgraphStruct.DgraphTask) error {
	held := false
	for _, l := range lists {
		held = held || len(*l) > 0
	}
	if !held {
		return nil
	}
	epics, err := EpicsIn(ctx, projects...)
	if err != nil {
		return err
	}
	dropEpics(epics, lists...)
	return nil
}

// dropEpics takes the tasks named in epics (and any nil) out of each list,
// in place. Pure.
func dropEpics(epics []string, lists ...*[]*dgraphStruct.DgraphTask) {
	if len(epics) == 0 {
		return
	}
	epic := make(map[string]bool, len(epics))
	for _, id := range epics {
		epic[strings.ToLower(id)] = true
	}
	for _, l := range lists {
		kept := (*l)[:0]
		for _, t := range *l {
			if t != nil && !epic[strings.ToLower(t.Uuid)] {
				kept = append(kept, t)
			}
		}
		*l = kept
	}
}

func topLevelOf(ctx context.Context, work bool, projects []string) (block, clause string, err error) {
	epics, err := EpicsIn(ctx, projects...)
	if err != nil {
		return "", "", err
	}
	block, clause = topLevel(epics, work)
	return block, clause, nil
}

// placeQuery reads what Place needs of the tasks named (list, from
// dgraphquery.UUIDList), as nodes of type Task only: a client's id never
// names another kind of node.
func placeQuery(list string) string {
	return `{
	q(func: eq(task_uuid, [` + list + `])) @filter(type(Task)) {
		uid
		task_uuid
		task_deleted_at
		task_project { project_uuid }
		task_parent_task { uid task_uuid }
		subs: count(task_sub_tasks @filter(` + dgraphStruct.TASK_LIVE_FILTER + `))
		waits: count(task_blocked_by @filter(` + dgraphStruct.TASK_LIVE_FILTER + `))
		blocks: count(~task_blocked_by @filter(` + dgraphStruct.TASK_LIVE_FILTER + `))
		task_sub_tasks @filter(` + dgraphStruct.TASK_LIVE_FILTER + `) {
			g as count(task_sub_tasks @filter(` + dgraphStruct.TASK_LIVE_FILTER + `))
			w as count(task_blocked_by @filter(` + dgraphStruct.TASK_LIVE_FILTER + `))
			b as count(~task_blocked_by @filter(` + dgraphStruct.TASK_LIVE_FILTER + `))
		}
		deep: sum(val(g))
		kid_waits: sum(val(w))
		kid_blocks: sum(val(b))
	}
}`
}

type placeRow struct {
	Uid       string     `json:"uid"`
	Uuid      string     `json:"task_uuid"`
	DeletedAt *time.Time `json:"task_deleted_at"`
	Project   *struct {
		Uuid string `json:"project_uuid"`
	} `json:"task_project"`
	Parent *struct {
		Uid  string `json:"uid"`
		Uuid string `json:"task_uuid"`
	} `json:"task_parent_task"`
	Subs      int     `json:"subs"`
	Waits     int     `json:"waits"`
	Blocks    int     `json:"blocks"`
	Deep      float64 `json:"deep"`
	KidWaits  float64 `json:"kid_waits"`
	KidBlocks float64 `json:"kid_blocks"`
}

// Places reads the live tasks named, by uuid, as the hierarchy sees them,
// each with its type and its parent's from Postgres; a task that isn't there,
// isn't a task or is deleted has none. read is dgexec.Read, or a
// transaction's ReadIn, so a change can decide on what it then writes.
func Places(ctx context.Context, read func(q string, vars map[string]string) (map[string][]placeRow, error), ids ...string) (map[string]Place, error) {
	list := dgraphquery.UUIDList(ids)
	out := map[string]Place{}
	if list == "" {
		return out, nil
	}
	got, err := read(placeQuery(list), nil)
	if err != nil {
		return nil, err
	}
	var typed []uuid.UUID
	for _, r := range got["q"] {
		if id, perr := uuid.Parse(r.Uuid); perr == nil {
			typed = append(typed, id)
		}
		if r.Parent != nil {
			if id, perr := uuid.Parse(r.Parent.Uuid); perr == nil {
				typed = append(typed, id)
			}
		}
	}
	facts, err := taskKeyModel.FactsFor(ctx, typed)
	if err != nil {
		return nil, err
	}
	epic := func(s string) bool {
		id, perr := uuid.Parse(s)
		f, ok := facts[id]
		return perr == nil && ok && f.Type != nil && f.Type.Builtin == "epic"
	}
	for _, r := range got["q"] {
		if r.Project == nil || r.Project.Uuid == "" || (r.DeletedAt != nil && r.DeletedAt.Year() > 1970) {
			continue
		}
		p := Place{UUID: r.Uuid, Uid: r.Uid, Project: r.Project.Uuid, Epic: epic(r.Uuid), SubTasks: r.Subs, Deep: r.Deep > 0,
			Dependent: r.Waits+r.Blocks > 0, KidsDependent: r.KidWaits+r.KidBlocks > 0}
		if r.Parent != nil {
			p.Parent, p.ParentUid, p.ParentEpic = r.Parent.Uuid, r.Parent.Uid, epic(r.Parent.Uuid)
		}
		out[r.Uuid] = p
	}
	return out, nil
}

// ReadPlaces is Places read on its own, outside any change.
func ReadPlaces(ctx context.Context, ids ...string) (map[string]Place, error) {
	return Places(ctx, func(q string, vars map[string]string) (map[string][]placeRow, error) {
		return dgexec.Read[map[string][]placeRow](ctx, q, vars)
	}, ids...)
}

// PlaceOf is one task's place: ErrHierarchyMissing when it isn't a live task.
func PlaceOf(ctx context.Context, id string) (Place, error) {
	got, err := ReadPlaces(ctx, id)
	if err != nil {
		return Place{}, err
	}
	p, ok := got[id]
	if !ok {
		return Place{}, fmt.Errorf("%w: %s", ErrHierarchyMissing, id)
	}
	return p, nil
}

// ErrHierarchyMissing is a task the hierarchy can't find: not there, not a
// task, or deleted.
var ErrHierarchyMissing = errors.New("tasks: that task isn't there")

// parentWrite is the write that moves the task (graph uid child) from under
// old to under parent ("" for either: at the top), on the one relation only:
// old lets it go, parent takes it, and the three are stamped
// (task_updated_at) so two moves that meet conflict and the second sees the
// first: two can't make a loop, or a place too deep, between them. Pure.
func parentWrite(child, old, parent string, at time.Time) (set, del []map[string]any) {
	me := map[string]any{"uid": child, "task_updated_at": at}
	set = append(set, me)
	if parent != "" {
		// A task holds one parent: setting it replaces the old one.
		me["task_parent_task"] = map[string]any{"uid": parent}
		set = append(set, map[string]any{"uid": parent, "task_sub_tasks": []map[string]any{{"uid": child}}, "task_updated_at": at})
	}
	if old != "" {
		set = append(set, map[string]any{"uid": old, "task_updated_at": at})
		if parent == "" {
			del = append(del, map[string]any{"uid": child, "task_parent_task": map[string]any{"uid": old}})
		}
		del = append(del, map[string]any{"uid": old, "task_sub_tasks": []map[string]any{{"uid": child}}})
	}
	return set, del
}

// ParentChange is what moving a task in the hierarchy has to check, read in
// the change's own transaction: the task, and the task it goes under (nil
// when it goes to the top), as the person may see them (nil: not there for
// them), with their places (nil: not a live task of a project). Each is read
// by its uuid as a Task node only: a client's id never names another kind of
// node, and the write takes the uids read here, never one it was given.
type ParentChange struct {
	Child, Parent           *dgraphStruct.DgraphTask
	ChildPlace, ParentPlace *Place
}

// ChangeParent moves the task childUUID under parentUUID ("": out to the
// top) on the one relation (parentWrite), once check has passed the change
// as it stands, in the same transaction; userDgraphUID is the person's graph
// uid, for their standing in the tasks' project. It reports whether anything
// changed: a task already where it is asked to go writes nothing. The move's
// history lines (moveHistory) go in the same mutation.
func ChangeParent(ctx context.Context, childUUID, parentUUID, userDgraphUID string, check func(*ParentChange) error) (bool, error) {
	changed := false
	err := dgraphInit.InTxn(ctx, func(txn *dgo.Txn) error {
		changed = false
		other := parentUUID
		if other == "" {
			other = childUUID
		}
		pair, err := readDependencyPair(ctx, dgoReader(ctx, txn), childUUID, other, userDgraphUID, false)
		if err != nil {
			return err
		}
		places, err := Places(ctx, func(q string, vars map[string]string) (map[string][]placeRow, error) {
			var out map[string][]placeRow
			err := dgraphInit.ReadIntoTxn(ctx, txn, q, vars, &out)
			return out, err
		}, childUUID, other)
		if err != nil {
			return err
		}
		c := &ParentChange{Child: pair.Task}
		if p, ok := places[childUUID]; ok {
			c.ChildPlace = &p
		}
		if parentUUID != "" {
			c.Parent = pair.Blocker
			if p, ok := places[parentUUID]; ok {
				c.ParentPlace = &p
			}
		}
		if err := check(c); err != nil {
			return err
		}
		// check passes only a live task as the person sees it, and the
		// uids written are those of the Task nodes read here.
		if c.Child == nil || c.ChildPlace == nil || c.Child.Uid != c.ChildPlace.Uid ||
			(parentUUID != "" && (c.Parent == nil || c.ParentPlace == nil || c.Parent.Uid != c.ParentPlace.Uid)) {
			return fmt.Errorf("%w: %s", ErrHierarchyMissing, childUUID)
		}
		if strings.EqualFold(c.ChildPlace.Parent, parentUUID) {
			return nil
		}
		newUid := ""
		if c.ParentPlace != nil {
			newUid = c.ParentPlace.Uid
		}
		at := time.Now().UTC()
		set, del := parentWrite(c.ChildPlace.Uid, c.ChildPlace.ParentUid, newUid, at)
		set = append(set, moveHistory(userDgraphUID, *c.ChildPlace, c.Child.Name, c.ParentPlace, at)...)
		req := &api.Request{}
		if len(del) > 0 {
			b, err := json.Marshal(del)
			if err != nil {
				return err
			}
			req.Mutations = append(req.Mutations, &api.Mutation{DeleteJson: b})
		}
		b, err := json.Marshal(set)
		if err != nil {
			return err
		}
		req.Mutations = append(req.Mutations, &api.Mutation{SetJson: b})
		if _, err := txn.Do(ctx, req); err != nil {
			helpers.LogErrorWithContext(ctx, "domain/ChangeParent err: %+v", err)
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}
