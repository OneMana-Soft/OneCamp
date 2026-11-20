package Domain

// Who may read a task. TaskReader.MayRead is the rule; business/Task
// CanViewTask asks it of one reader, and Readers here of many at once: the
// people a task's notices may reach and its comments may mention, so an email
// about a task reaches only someone the app would show it to.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Task"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// Readers is which of the people (user ids, matched in any case and answered
// as given) may read the task now; a task that isn't there has none. One read
// of the graph and one of the task's restriction: a restricted task is read
// only by its project's admins, its assignee, its creator and the people
// granted on it (business/WorkAccess LetIn), so a notice about it reaches no
// one else.
func Readers(ctx context.Context, taskUUID string, userUUIDs []string) (map[string]bool, error) {
	_, readers, _, err := readersOf(ctx, taskUUID, userUUIDs)
	return readers, err
}

// Seeing is how one person stands to reading a task as it is: whether they
// may (Readers), whether it is in a project, and whether its restriction is
// what keeps them out (one of its project's people it isn't let in to).
type Seeing struct {
	Sees, InProject, Restricted bool
}

// SeeingOf is Seeing for one person (a user id), in the reads Readers makes.
// A task that isn't there is seen by no one.
func SeeingOf(ctx context.Context, taskUUID, userUUID string) (Seeing, error) {
	task, readers, kept, err := readersOf(ctx, taskUUID, []string{userUUID})
	if err != nil || task == nil {
		return Seeing{}, err
	}
	return Seeing{Sees: readers[userUUID], InProject: task.Project != nil, Restricted: kept[userUUID]}, nil
}

// readersOf is Readers, with the task as read and who of the people its
// restriction kept out.
func readersOf(ctx context.Context, taskUUID string, userUUIDs []string) (*dgraphStruct.DgraphTask, map[string]bool, map[string]bool, error) {
	task, readers, err := projectReaders(ctx, taskUUID, userUUIDs)
	if err != nil || task == nil || len(readers) == 0 {
		return task, readers, nil, err
	}
	id, err := uuid.Parse(task.Uuid)
	if err != nil {
		return task, readers, nil, nil
	}
	r, err := restrictionOf(ctx, id)
	if err != nil {
		return task, nil, nil, err
	}
	readers, kept, err := restricted(ctx, task, readers, r, map[string]taskread.Filter{})
	return task, readers, kept, err
}

// restricted is readers less whoever r, the task's restriction, keeps out,
// and who that is; with none, readers as they are. A restricted task is read
// only by its project's admins, its assignee, its creator and the people
// granted on it (business/WorkAccess LetIn). filters keeps each person's task
// filter (models/taskread) once decided, by lower-case id, for the next task.
func restricted(ctx context.Context, task *dgraphStruct.DgraphTask, readers map[string]bool, r restrictionModel.Restriction, filters map[string]taskread.Filter) (map[string]bool, map[string]bool, error) {
	if !r.Restricted {
		return readers, nil, nil
	}
	granted := make(map[string]bool, len(r.Grants))
	for _, g := range r.Grants {
		granted[g.String()] = true
	}
	is := func(u *dgraphStruct.DgraphUser, who string) bool {
		return u != nil && u.Uuid != "" && strings.EqualFold(u.Uuid, who)
	}
	kept := map[string]bool{}
	for who := range readers {
		key := strings.ToLower(who)
		if granted[key] || is(task.Assignee, who) || is(task.CreatedBy, who) {
			continue
		}
		// Anyone else only as one of its project's admins, by the whole rule
		// (a role row can say less than the graph's admin edge).
		f, decided := filters[key]
		if !decided {
			var err error
			if f, err = taskread.For(ctx, who); err != nil {
				return nil, nil, err
			}
			filters[key] = f
		}
		if f.Hides(task.Uuid) {
			delete(readers, who)
			kept[who] = true
		}
	}
	return readers, kept, nil
}

// ReadersOfMany is Readers for many tasks at once: by each task's uuid as
// asked, which of its people (user ids, answered as given) may read it now.
// A task that isn't there, or is deleted, has none: what an email about it
// asks at its send (Readers reads a deleted task as it was). One graph read
// and one of the restrictions for them all; a restricted task's people are
// asked of their task filters (restricted), each person once.
func ReadersOfMany(ctx context.Context, people map[string][]string) (map[string]map[string]bool, error) {
	out := make(map[string]map[string]bool, len(people))
	asked := make(map[string]string, len(people)) // by lower-case uuid: the task as asked
	var ids []string
	for id, who := range people {
		out[id] = map[string]bool{}
		if _, err := uuid.Parse(id); err == nil && len(who) > 0 {
			asked[strings.ToLower(id)] = id
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	list, _ := json.Marshal(ids)
	query := `{
				taskInfo(func: eq(task_uuid, ` + string(list) + `)) @filter(` + dgraphquery.Live("task_deleted_at") + `) {` + readersFields + `}
			}`
	tasks, err := dgraphModels.QueryDgraphTasks(taskread.WithSystem(ctx), query, nil)
	if err != nil {
		return nil, err
	}
	found := make(map[uuid.UUID]*dgraphStruct.DgraphTask, len(tasks))
	var read []uuid.UUID
	for _, task := range tasks {
		key, ok := asked[strings.ToLower(task.Uuid)]
		id, err := uuid.Parse(task.Uuid)
		if !ok || err != nil {
			continue
		}
		if out[key] = ReadersAmong(task, people[key]); len(out[key]) > 0 {
			found[id] = task
			read = append(read, id)
		}
	}
	if len(read) == 0 {
		return out, nil
	}
	rs, err := restrictionModel.OfMany(ctx, read)
	if err != nil {
		return nil, err
	}
	filters := map[string]taskread.Filter{}
	for id, task := range found {
		key := asked[strings.ToLower(task.Uuid)]
		if out[key], _, err = restricted(ctx, task, out[key], rs[id], filters); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// MemberReaders is which of the people may read the task (Readers) as one of
// its project's members or admins: whom its project's news is for. Someone
// taken off the project still reads a task given to them (MayRead: nobody is
// locked out of their own work), but nothing about the project reaches them.
// A task in no project has none. One read, as Readers.
func MemberReaders(ctx context.Context, taskUUID string, userUUIDs []string) (map[string]bool, error) {
	task, readers, _, err := readersOf(ctx, taskUUID, userUUIDs)
	if err != nil || task == nil {
		return map[string]bool{}, err
	}
	return onlyMembers(task, readers), nil
}

// onlyMembers is readers (user ids, as asked) less whoever isn't one of the
// task's project's members or admins: its assignee and its creator are
// readers by being them, not by this. Pure.
func onlyMembers(task *dgraphStruct.DgraphTask, readers map[string]bool) map[string]bool {
	ids := make([]string, 0, len(readers))
	for who := range readers {
		ids = append(ids, who)
	}
	people := ReadersAmong(&dgraphStruct.DgraphTask{Project: task.Project}, ids)
	out := make(map[string]bool, len(people))
	for who := range readers {
		if people[who] {
			out[who] = true
		}
	}
	return out
}

// InProject is which of the people are the task's project's people, or its
// assignee: who may be let into it by name when it is restricted. One read.
func InProject(ctx context.Context, taskUUID string, userUUIDs []string) (map[string]bool, error) {
	_, readers, err := projectReaders(ctx, taskUUID, userUUIDs)
	return readers, err
}

// restrictionOf is a seam for tests.
var restrictionOf = restrictionModel.Of

// projectReaders is which of the people its project lets read the task, with
// the task as read for that, before any restriction.
func projectReaders(ctx context.Context, taskUUID string, userUUIDs []string) (*dgraphStruct.DgraphTask, map[string]bool, error) {
	if len(userUUIDs) == 0 {
		return nil, map[string]bool{}, nil
	}
	query := `query TaskReaders($id: string){
				taskInfo(func: eq(task_uuid, $id)) {` + readersFields + `}
			}`
	// Who may read it is decided for others than the reader of ctx, so the
	// read itself sees the task whatever ctx's reader may (models/taskread).
	task, err := dgraphModels.GetDgraphTaskInfoByUUID(taskread.WithSystem(ctx), query, map[string]string{"$id": taskUUID})
	if errors.Is(err, dgraphModels.ErrNotFound) {
		return nil, map[string]bool{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return task, ReadersAmong(task, userUUIDs), nil
}

// readersFields is what a task is read with to decide its readers
// (ReadersAmong). The project's uid and uuid keep its block from reading as
// no project when nobody is in it.
const readersFields = `
					task_uuid
					task_assignee { user_uuid }
					task_created_by { user_uuid }
					task_project {
						uid
						project_uuid
						project_members { user_uuid }
						project_admins { user_uuid }
					}
				`

// TaskReader is how one person stands to a task.
type TaskReader struct {
	Member, Admin, Assignee, Creator bool
}

// MayRead is the task read rule. The assignee reads it, in its project or
// not, so nobody is locked out of their own work. A task in a project is read
// by the project's members and admins: its creator only as one of them, since
// only a project's admins create its tasks and a creator outside the project
// was taken off it (leaving a project leaves its work). A task in no project
// belongs to whoever made it. Pure.
func (r TaskReader) MayRead(inProject bool) bool {
	if r.Assignee {
		return true
	}
	if inProject {
		return r.Member || r.Admin
	}
	return r.Creator
}

// ReadersAmong is which of the people may read the task (MayRead), read with
// its project's members and admins, its assignee and its creator. Pure.
func ReadersAmong(task *dgraphStruct.DgraphTask, userUUIDs []string) map[string]bool {
	out := make(map[string]bool, len(userUUIDs))
	if task == nil {
		return out
	}
	set := func(users []*dgraphStruct.DgraphUser) map[string]bool {
		s := make(map[string]bool, len(users))
		for _, u := range users {
			if u != nil && u.Uuid != "" {
				s[strings.ToLower(u.Uuid)] = true
			}
		}
		return s
	}
	is := func(u *dgraphStruct.DgraphUser, id string) bool {
		return u != nil && u.Uuid != "" && strings.EqualFold(u.Uuid, id)
	}
	var members, admins map[string]bool
	if p := task.Project; p != nil {
		members, admins = set(p.Members), set(p.Admins)
	}
	for _, id := range userUUIDs {
		key := strings.ToLower(id)
		reader := TaskReader{Member: members[key], Admin: admins[key], Assignee: is(task.Assignee, id), Creator: is(task.CreatedBy, id)}
		if reader.MayRead(task.Project != nil) {
			out[id] = true
		}
	}
	return out
}
