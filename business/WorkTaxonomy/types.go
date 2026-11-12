// Package business (WorkTaxonomy) is the Work Taxonomy
// (design/projects-vs-asana-jira/PROGRAM.md §4.2 #95, D-PJ-9; projects A8K):
// what kind of work a task is. Seven types are built in (Task, Bug, Feature,
// Milestone, Approval, Request and Epic) and a project's admins add their
// own, each a name and an icon. A task with no type is a Task. An epic holds
// tasks through the one parent/sub-task relation (domain/Task hierarchy.go):
// epic -> task -> sub-task.
//
// A type shows as a glyph beside the task's key and as the panel's Type row,
// and milestones draw as diamonds on the timeline. Labels with groups and
// estimate scales join this package once Field Types (#60) is on beta.
package business

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	"github.com/google/uuid"
)

// The built-in types' ids, as the task_keys_and_types migration (471) made them.
var (
	TypeTask      = uuid.MustParse("a2573bba-d50b-44a2-b9f0-94df8d7310a7")
	TypeBug       = uuid.MustParse("5594da4f-672d-4b63-8887-3d4b83ae9cb5")
	TypeFeature   = uuid.MustParse("f33995b9-59c7-4e4e-8167-a6c205ca9ca9")
	TypeMilestone = uuid.MustParse("fd0c7ea2-3255-4a4e-9d50-97c30dea1b61")
	TypeApproval  = uuid.MustParse("38e9401a-79ea-481d-9af1-b5411003aecd")
	TypeRequest   = uuid.MustParse("65e4944a-7ee3-436f-aceb-5f90dfc1c60d")
	// TypeEpic is the seventh, from task_type_epic (582).
	TypeEpic = uuid.MustParse("0c06e5fa-fd1a-41d1-bb38-7cdb1fe8097e")
)

// Icons is the glyphs a project's own type may take; the app draws each.
var Icons = []string{
	"task", "bug", "feature", "milestone", "approval", "request", "epic",
	"flag", "book", "wrench", "shield", "palette", "megaphone",
	"flask", "layers", "target", "rocket", "file", "chart",
}

// The limits a project's types keep.
const (
	MaxNameLen = 40
	// MaxTypes is how many types of its own a project may have at once.
	MaxTypes = 20
	// MaxEpics is how many epics a project holds: the top-level reads name
	// them (domain/Task TopLevel), so the number is bounded.
	MaxEpics = taskDomain.MaxEpics
)

// ErrBadType is a name or icon a type can't have; its message says why in
// words.
var ErrBadType = errors.New("that type can't be saved")

// The model's errors, for the controllers' words.
var (
	ErrTypeNameTaken = model.ErrTypeNameTaken
	ErrNoType        = model.ErrNoType
	// ErrHierarchy is a type change the work hierarchy refuses (an epic sits
	// at the top), in words.
	ErrHierarchy = taskDomain.ErrHierarchy
)

// Seams for tests.
var (
	listTypes   = model.Types
	createType  = model.CreateType
	updateType  = model.UpdateType
	archiveType = model.ArchiveType
	setTaskType = model.SetTaskType
	typedTasks  = model.TypedTasks
	writeLine   = taskDomain.UpdateDgraphTaskClearing
	factsFor    = model.FactsFor
	placeOf     = taskDomain.PlaceOf
	epicsIn     = model.Epics
)

func bad(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadType, fmt.Sprintf(format, args...))
}

// Clean is a type's name and icon as they're kept: the name trimmed, one to
// MaxNameLen characters, the icon one of Icons.
func Clean(name, icon string) (string, string, error) {
	n := strings.Join(strings.Fields(name), " ")
	if n == "" {
		return "", "", bad("Give the type a name.")
	}
	if utf8.RuneCountInString(n) > MaxNameLen {
		return "", "", bad("A type's name is at most %d characters.", MaxNameLen)
	}
	for _, i := range Icons {
		if i == icon {
			return n, icon, nil
		}
	}
	return "", "", bad("Pick one of the icons offered.")
}

// Types is the types a task in project can have: the built-in ones, then the
// project's own. uuid.Nil, no project, has the built-in ones alone.
func Types(ctx context.Context, projectID uuid.UUID) ([]model.Type, error) {
	return listTypes(ctx, projectID, false)
}

// Answer is a type as the app reads it: Builtin names a built-in one, Own
// marks a project's own.
type Answer struct {
	ID      string `json:"id"`
	Builtin string `json:"builtin,omitempty"`
	Name    string `json:"name"`
	Icon    string `json:"icon"`
	Own     bool   `json:"own,omitempty"`
}

// AnswerOf is t as the app reads it.
func AnswerOf(t model.Type) Answer {
	return Answer{ID: t.ID.String(), Builtin: t.Builtin, Name: t.Name, Icon: t.Icon, Own: t.ProjectID != nil}
}

// Answers is types as the app reads them, in their order; none is an empty
// list, never null.
func Answers(types []model.Type) []Answer {
	out := make([]Answer, 0, len(types))
	for _, t := range types {
		out = append(out, AnswerOf(t))
	}
	return out
}

// Create adds a type to a project.
func Create(ctx context.Context, projectID uuid.UUID, name, icon string, by uuid.UUID) (model.Type, error) {
	name, icon, err := Clean(name, icon)
	if err != nil {
		return model.Type{}, err
	}
	all, err := listTypes(ctx, projectID, false)
	if err != nil {
		return model.Type{}, err
	}
	own := 0
	for _, t := range all {
		if t.ProjectID != nil {
			own++
		}
		if strings.EqualFold(t.Name, name) {
			return model.Type{}, ErrTypeNameTaken
		}
	}
	if own >= MaxTypes {
		return model.Type{}, bad("A project has at most %d types of its own. Archive one to add another.", MaxTypes)
	}
	return createType(ctx, projectID, name, icon, by)
}

// Update renames a project's own type and changes its icon. The built-in
// ones can't be changed.
func Update(ctx context.Context, projectID, typeID uuid.UUID, name, icon string) error {
	name, icon, err := Clean(name, icon)
	if err != nil {
		return err
	}
	if Builtin(typeID) {
		return bad("The built-in types can't be renamed.")
	}
	all, err := listTypes(ctx, projectID, false)
	if err != nil {
		return err
	}
	for _, t := range all {
		if t.ID != typeID && strings.EqualFold(t.Name, name) {
			return ErrTypeNameTaken
		}
	}
	return updateType(ctx, projectID, typeID, name, icon)
}

// Archive takes a project's own type out of the pickers; its tasks keep it.
func Archive(ctx context.Context, projectID, typeID uuid.UUID) error {
	if Builtin(typeID) {
		return bad("The built-in types can't be archived.")
	}
	return archiveType(ctx, projectID, typeID)
}

// FilterID is the list's and the board's filter by type (linear-03): its
// values are type ids, and Task's stands for every plain task.
const FilterID = "task_work_type"

// FilterClause narrows a project's task list to the tasks of the given types.
// The clause names typed tasks only, never the plain ones, so its size is
// bounded by the project's typed work: with Task chosen it leaves out the
// typed tasks of the other types; without it, it keeps the tasks of the
// chosen ones. An id that is no type here matches nothing. "" narrows
// nothing (Task chosen, and no other type in use).
func FilterClause(ctx context.Context, projectID uuid.UUID, ids []string) (string, error) {
	var chosen []uuid.UUID
	plain := false
	for _, raw := range ids {
		switch id, err := uuid.Parse(raw); {
		case err != nil:
		case id == TypeTask:
			plain = true
		default:
			chosen = append(chosen, id)
		}
	}
	if !plain && len(chosen) == 0 {
		return taskDomain.NoTask, nil
	}
	tasks, err := typedTasks(ctx, projectID, chosen, plain)
	if err != nil {
		return "", err
	}
	return taskDomain.TasksClause(tasks, plain), nil
}

// Builtins is the built-in types' ids in their position order, the one list
// of them: task_keys_and_types (471) made the first six, task_type_epic (582)
// Epic. A new built-in type is added here and nowhere else in Go.
func Builtins() []uuid.UUID {
	return []uuid.UUID{TypeTask, TypeBug, TypeFeature, TypeMilestone, TypeApproval, TypeRequest, TypeEpic}
}

// Builtin says whether id is one of the built-in types.
func Builtin(id uuid.UUID) bool {
	return slices.Contains(Builtins(), id)
}

// SetTaskType gives a task a type (nil: a Task), as by, and writes the change
// into its history. The type is a built-in one or the task's project's own.
// Answers the type it has now (nil for a Task). The caller has checked that
// by may change the task.
func SetTaskType(ctx context.Context, task *dgraphStruct.DgraphTask, typeID *uuid.UUID, by *dgraphStruct.DgraphUser) (*model.Type, error) {
	taskID, err := uuid.Parse(task.Uuid)
	if err != nil {
		return nil, model.ErrNoTask
	}
	if typeID != nil && *typeID == TypeTask {
		typeID = nil
	}
	if err := mayRetype(ctx, taskID, typeID != nil && *typeID == TypeEpic); err != nil {
		return nil, err
	}
	before, after, err := setTaskType(ctx, taskID, typeID)
	if err != nil {
		return nil, err
	}
	if sameType(before, after) {
		return after, nil
	}
	now := time.Now()
	line := &dgraphStruct.DgraphTask{Uid: "uid(task)", Uuid: task.Uuid, UpdatedAt: &now,
		Activity: []*dgraphStruct.DgraphTaskActivity{{
			Uuid:      uuid.NewString(),
			Type:      dgraphStruct.ACTIVITY_TYPE_WORK_TYPE,
			CreatedBy: &dgraphStruct.DgraphUser{Uid: by.Uid},
			LogTime:   &now,
			PrevState: before.Describe(),
			NextState: after.Describe(),
		}}}
	// The type is saved; a history line that didn't make it is logged, not
	// turned into a failure the person would retry.
	if err := writeLine(ctx, line, nil); err != nil {
		helpers.LogErrorWithContext(ctx, "business/WorkTaxonomy SetTaskType history err: %+v", err)
	}
	return after, nil
}

// mayRetype is nil when the task may become an epic (epic) or stop being
// one, by the hierarchy's rule (domain/Task MayType): an epic sits at the
// top, and stays one while its tasks hold sub-tasks or take dependencies. A
// project holds at most MaxEpics. A change that keeps it an epic, or keeps it
// not one, reads nothing more.
func mayRetype(ctx context.Context, taskID uuid.UUID, epic bool) error {
	facts, err := factsFor(ctx, []uuid.UUID{taskID})
	if err != nil {
		return err
	}
	f := facts[taskID]
	if (f.Type != nil && f.Type.ID == TypeEpic) == epic {
		return nil
	}
	place, err := placeOf(ctx, taskID.String())
	if errors.Is(err, taskDomain.ErrHierarchyMissing) {
		return model.ErrNoTask
	}
	if err != nil {
		return err
	}
	if err := taskDomain.MayType(place, epic); err != nil {
		return err
	}
	if !epic {
		return nil
	}
	project, err := uuid.Parse(place.Project)
	if err != nil {
		return model.ErrNoTask
	}
	epics, err := epicsIn(ctx, []uuid.UUID{project})
	if err != nil {
		return err
	}
	if len(epics) >= MaxEpics {
		return bad("A project holds at most %d epics.", MaxEpics)
	}
	return nil
}

func sameType(a, b *model.Type) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.ID == b.ID
}
