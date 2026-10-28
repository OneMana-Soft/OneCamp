package Domain

// Who may read a task. TaskReader.MayRead is the rule; business/Task
// CanViewTask asks it of one reader, and Readers here of many at once: the
// people a task's notices may reach and its comments may mention, so an email
// about a task reaches only someone the app would show it to.

import (
	"context"
	"errors"
	"strings"

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
	if !r.Restricted {
		return task, readers, nil, nil
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
		f, err := taskread.For(ctx, who)
		if err != nil {
			return task, nil, nil, err
		}
		if f.Hides(task.Uuid) {
			delete(readers, who)
			kept[who] = true
		}
	}
	return task, readers, kept, nil
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
	// The project's uid and uuid keep its block from reading as no project
	// when nobody is in it.
	query := `query TaskReaders($id: string){
				taskInfo(func: eq(task_uuid, $id)) {
					task_uuid
					task_assignee { user_uuid }
					task_created_by { user_uuid }
					task_project {
						uid
						project_uuid
						project_members { user_uuid }
						project_admins { user_uuid }
					}
				}
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
