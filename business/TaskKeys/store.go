package business

import (
	"context"
	"errors"
	"strconv"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	"github.com/google/uuid"
)

// The errors callers turn into words.
var (
	// ErrKeyTaken is a key another project uses, or used before.
	ErrKeyTaken = model.ErrKeyTaken
	// ErrNoTask is a key, or an id, that names no task.
	ErrNoTask = model.ErrNoTask
	// ErrNoKey is a project with no key yet.
	ErrNoKey = model.ErrNoKey
)

// Seams for tests.
var (
	ensureKey     = model.EnsureKey
	taken         = model.Taken
	numberProject = model.NumberUnnumbered
	projectName   = model.ProjectName
	resolve       = model.Resolve
	keepAliases   = model.KeepAliases
	factsFor      = model.FactsFor
	projectKeys   = model.ProjectKeys
)

// Ensure gives a project its key from its name, the first that no other
// project uses or used, and answers the key it has (one it already had
// stands). name "" reads the project's own name.
func Ensure(ctx context.Context, projectID uuid.UUID, name string) (string, error) {
	if name == "" {
		n, err := projectName(ctx, projectID)
		if err != nil {
			return "", err
		}
		name = n
	}
	candidates := Derive(name)
	// One read leaves out the candidates other projects have, so the insert
	// usually lands first time; one taken meanwhile just moves it on.
	if busy, err := taken(ctx, projectID, candidates); err == nil && len(busy) > 0 {
		free := candidates[:0:0]
		for _, c := range candidates {
			if !busy[c] {
				free = append(free, c)
			}
		}
		candidates = free
	}
	return ensureKey(ctx, projectID, candidates)
}

// Settle makes sure a project has a key and that every task in it has a
// number: for a task made in a project that had no key yet (a project made
// before keys existed, until the projects.keys sweep reaches it).
func Settle(ctx context.Context, projectID uuid.UUID) error {
	if _, err := Ensure(ctx, projectID, ""); err != nil {
		return err
	}
	_, err := numberProject(ctx, projectID)
	return err
}

// Resolve is the task a key names ("Q4-12", "q4-12", a key its project used
// before, or one the task had in the tool it was imported from: Linear's
// ENG-7). It doesn't ask whether anyone may open the task: its caller does,
// and answers a task they can't open as one that isn't there.
func Resolve(ctx context.Context, raw string) (uuid.UUID, error) {
	k, n, ok := ParseSource(raw)
	if !ok {
		return uuid.Nil, ErrNoTask
	}
	return resolve(ctx, k, n)
}

// KeepSourceKeys makes the keys a task had in the tool it was imported from
// (raws: Linear's ENG-7, and those it had before it moved teams) find it,
// beside its own key, when its project's key differs (O-L2). It answers the
// keys it left as they were: one a project here uses or used, or one another
// task already answers to. A raw no key could be is left out, unanswered.
// Rolling importID back puts the task, and so its keys, in the trash.
func KeepSourceKeys(ctx context.Context, raws []string, task, importID uuid.UUID) (held []string, err error) {
	var keys []model.AliasKey
	for _, raw := range raws {
		if k, n, ok := ParseSource(raw); ok {
			keys = append(keys, model.AliasKey{Key: k, Number: n})
		}
	}
	got, err := keepAliases(ctx, keys, task, importID)
	for _, k := range got {
		held = append(held, k.Key+"-"+strconv.Itoa(k.Number))
	}
	return held, err
}

// Ref is the task raw names: its id, or its key. As with Resolve, the caller
// checks the reader may open it.
func Ref(ctx context.Context, raw string) (uuid.UUID, error) {
	if id, err := uuid.Parse(raw); err == nil {
		return id, nil
	}
	return Resolve(ctx, raw)
}

// Lay sets each task's key and type, and those of the tasks it holds (its
// sub-tasks, its parent, what it waits on and what waits on it), from one
// read, for an answer to the app. A task with no row, or a read that failed,
// keeps none: the answer still goes, without keys, and the failure is logged.
// A task the reader can't see (a stub, models/taskread) keeps none either: it
// says only that it is there.
func Lay(ctx context.Context, tasks ...*dgraphStruct.DgraphTask) {
	type read struct {
		t  *dgraphStruct.DgraphTask
		id uuid.UUID
	}
	var all []read
	var ids []uuid.UUID
	seen := map[*dgraphStruct.DgraphTask]bool{}
	var walk func(t *dgraphStruct.DgraphTask, depth int)
	walk = func(t *dgraphStruct.DgraphTask, depth int) {
		if t == nil || seen[t] || t.Hidden {
			return
		}
		seen[t] = true
		if id, err := uuid.Parse(t.Uuid); err == nil {
			all = append(all, read{t, id})
			ids = append(ids, id)
		}
		if depth > 0 {
			return
		}
		walk(t.ParentTask, depth+1)
		for _, list := range [][]*dgraphStruct.DgraphTask{t.SubTasks, t.BlockedBy, t.Blocks} {
			for _, o := range list {
				walk(o, depth+1)
			}
		}
	}
	for _, t := range tasks {
		walk(t, 0)
	}
	if len(ids) == 0 {
		return
	}
	facts, err := factsFor(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/TaskKeys.Lay couldn't read %d tasks' keys and types: %+v", len(ids), err)
		return
	}
	for _, r := range all {
		f, ok := facts[r.id]
		if !ok {
			continue
		}
		r.t.Key = f.Key
		r.t.WorkType = nil
		if f.Type != nil {
			r.t.WorkType = &dgraphStruct.TaskWorkType{ID: f.Type.ID.String(), Builtin: f.Type.Builtin, Name: f.Type.Name, Icon: f.Type.Icon}
		}
	}
}

// LayProjects sets each project's key on an answer that lists projects (a
// new task's address names its project by key), in one read: a project with
// no key yet, or a read that fails (logged), has none.
func LayProjects(ctx context.Context, projects ...*dgraphStruct.DgraphProject) {
	ids := make([]uuid.UUID, 0, len(projects))
	for _, p := range projects {
		if id, err := uuid.Parse(projectID(p)); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	keys, err := projectKeys(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "TaskKeys: reading the keys of %d projects: %v", len(ids), err)
		return
	}
	for _, p := range projects {
		if id, err := uuid.Parse(projectID(p)); err == nil {
			p.Key = keys[id]
		}
	}
}

// projectID is a listed project's id, "" for none.
func projectID(p *dgraphStruct.DgraphProject) string {
	if p == nil {
		return ""
	}
	return p.Uuid
}

// KeysOf is the key of each of tasks that has one, by id: for answers that
// aren't graph tasks (search hits).
func KeysOf(ctx context.Context, tasks []uuid.UUID) (map[uuid.UUID]string, error) {
	facts, err := factsFor(ctx, tasks)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(facts))
	for id, f := range facts {
		if f.Key != "" {
			out[id] = f.Key
		}
	}
	return out, nil
}

// KeyOf is one task's key, for an answer that names the task (the API's, an
// agent's tool's): "" when its project has no key yet, or when it can't be
// read now (logged), and the answer names the task without it.
func KeyOf(ctx context.Context, task uuid.UUID) string {
	keys, err := KeysOf(ctx, []uuid.UUID{task})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "TaskKeys: reading the key of task %s: %v", task, err)
		return ""
	}
	return keys[task]
}

// Named is how an answer names a task with its key first, as Linear names an
// issue (Q4-12 "Write the launch post"): named as the answer quotes it, and
// left as it is when there's no key.
func Named(key, named string) string {
	if key == "" {
		return named
	}
	return key + " " + named
}

// NamedAll is Named for tasks' names by id, in one read: what a notice calls
// each (Q4-12 Fix the login). A name left empty (one its reader can't see)
// stays empty, keyless; so does every name when the read fails (logged).
func NamedAll(ctx context.Context, names map[string]string) map[string]string {
	ids := make([]uuid.UUID, 0, len(names))
	for id, name := range names {
		if parsed, err := uuid.Parse(id); err == nil && name != "" {
			ids = append(ids, parsed)
		}
	}
	if len(ids) == 0 {
		return names
	}
	keys, err := KeysOf(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "TaskKeys: reading the keys of %d named tasks: %v", len(ids), err)
		return names
	}
	out := make(map[string]string, len(names))
	for id, name := range names {
		out[id] = name
		if parsed, err := uuid.Parse(id); err == nil && name != "" {
			out[id] = Named(keys[parsed], name)
		}
	}
	return out
}

// NamedTask is NamedAll for one task.
func NamedTask(ctx context.Context, id, name string) string {
	return NamedAll(ctx, map[string]string{id: name})[id]
}

// SetKey changes a project's key as typed, by whom, keeping the one it had
// as an alias. Answers the key it had and the one it has now. A bad key is
// ErrBadKey, one another project has or had ErrKeyTaken; a project that has
// no key yet gets one first.
func SetKey(ctx context.Context, projectID uuid.UUID, typed string, by uuid.UUID) (old, now string, err error) {
	now, err = Valid(typed)
	if err != nil {
		return "", "", err
	}
	old, err = model.SetKey(ctx, projectID, now, by)
	if errors.Is(err, model.ErrNoKey) {
		if _, err = Ensure(ctx, projectID, ""); err != nil {
			return "", "", err
		}
		old, err = model.SetKey(ctx, projectID, now, by)
	}
	return old, now, err
}

// TakeOver gives a project key, taking it from the project that has it or
// had it: for a project made to replace another, as the demo seed's nightly
// refresh does, so its keys read the same every day. Never offered to people,
// whose keys stay theirs.
func TakeOver(ctx context.Context, projectID uuid.UUID, key string) error {
	k, err := Valid(key)
	if err != nil {
		return err
	}
	err = model.TakeKey(ctx, projectID, k)
	if errors.Is(err, model.ErrNoKey) {
		if _, err = Ensure(ctx, projectID, ""); err != nil {
			return err
		}
		err = model.TakeKey(ctx, projectID, k)
	}
	return err
}

// Of is a project's key, the number its next task takes, and the keys it used
// before. A project with no key yet gets one first.
func Of(ctx context.Context, projectID uuid.UUID) (key string, next int, aliases []string, err error) {
	key, next, err = model.Key(ctx, projectID)
	if errors.Is(err, model.ErrNoKey) {
		if key, err = Ensure(ctx, projectID, ""); err != nil {
			return "", 0, nil, err
		}
		_, next, err = model.Key(ctx, projectID)
	}
	if err != nil {
		return "", 0, nil, err
	}
	aliases, err = model.Aliases(ctx, projectID)
	return key, next, aliases, err
}
