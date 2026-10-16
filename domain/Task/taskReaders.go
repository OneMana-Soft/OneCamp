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
)

// Readers is which of the people (user ids, matched in any case and answered
// as given) may read the task now; a task that isn't there has none. One read
// of the graph.
func Readers(ctx context.Context, taskUUID string, userUUIDs []string) (map[string]bool, error) {
	if len(userUUIDs) == 0 {
		return map[string]bool{}, nil
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
	task, err := dgraphModels.GetDgraphTaskInfoByUUID(ctx, query, map[string]string{"$id": taskUUID})
	if errors.Is(err, dgraphModels.ErrNotFound) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	return ReadersAmong(task, userUUIDs), nil
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
