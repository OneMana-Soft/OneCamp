package entityLinkDomain

// Reads for Live Entity Chips (docs programme §4 #21, built by C1): what a
// chip shows of docs, tasks and projects, each kind in one query however many
// chips ask. None of them decides who may see what: business/EntityLink asks
// the Access Resolver about every doc, and the task view's own rule about
// every task and project, before anything here reaches a reader.

import (
	"context"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Doc"
	projectModels "github.com/akashc777/OneCamp/models/dgraph/Project"
	taskModels "github.com/akashc777/OneCamp/models/dgraph/Task"
)

// DocChips reads what a chip shows of each doc (its title, icon, parent and
// space, and what the node is and its colour when it isn't a plain page:
// docs F1's folders, files and links), keyed by lower-case uuid, in one
// query. A doc that isn't there is left out.
func DocChips(ctx context.Context, docUUIDs []string) (map[string]*dgraphStruct.DgraphDoc, error) {
	out := map[string]*dgraphStruct.DgraphDoc{}
	list := dgraphquery.UUIDList(docUUIDs)
	if list == "" {
		return out, nil
	}
	docs, err := dgraphModels.GetDgraphDocs(ctx, `{
		docInfo(func: eq(doc_uuid, [`+list+`])) {
			doc_uuid
			doc_title
			doc_icon
			doc_node_kind
			doc_link_url
			doc_color
			doc_space_kind
			doc_space_uuid
			doc_parent { doc_uuid }
		}
	}`, map[string]string{})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/DocChips err: %+v", err)
		return nil, err
	}
	for _, d := range docs {
		if d != nil && d.Uuid != "" {
			out[strings.ToLower(d.Uuid)] = d
		}
	}
	return out, nil
}

// TaskChips reads what a chip shows of each task, with what the task view
// decides by for one reader (their membership and adminship of its project,
// its assignee and its maker) and whether it or its project was deleted, in
// one query. A task that isn't there is left out.
func TaskChips(ctx context.Context, taskUUIDs []string, readerUID string) ([]*dgraphStruct.DgraphTask, error) {
	list := dgraphquery.UUIDList(taskUUIDs)
	if list == "" {
		return nil, nil
	}
	tasks, err := taskModels.QueryDgraphTasks(ctx, `query chips($userUid: string) {
		taskInfo(func: eq(task_uuid, [`+list+`])) {
			task_uuid
			task_name
			task_status
			task_custom_status
			task_custom_status_name
			task_due_date
			task_deleted_at
			task_project {
				project_uuid
				project_name
				project_deleted_at
				project_is_member: count(project_members @filter(uid($userUid)))
				project_is_admin: count(project_admins @filter(uid($userUid)))
			}
			task_assignee { uid user_uuid user_name user_full_name user_profile_object_key }
			task_created_by { uid user_uuid }
		}
	}`, map[string]string{"$userUid": readerUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/TaskChips err: %+v", err)
	}
	return tasks, err
}

// ProjectChips reads what a chip shows of each project, with whether one
// reader is its member or admin, in one query. A project that isn't there is
// left out.
func ProjectChips(ctx context.Context, projectUUIDs []string, readerUID string) ([]*dgraphStruct.DgraphProject, error) {
	list := dgraphquery.UUIDList(projectUUIDs)
	if list == "" {
		return nil, nil
	}
	projects, err := projectModels.GetDgraphProjectList(ctx, `query chips($userUid: string) {
		projectInfo(func: eq(project_uuid, [`+list+`])) {
			project_uuid
			project_name
			project_deleted_at
			project_is_member: count(project_members @filter(uid($userUid)))
			project_is_admin: count(project_admins @filter(uid($userUid)))
		}
	}`, map[string]string{"$userUid": readerUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ProjectChips err: %+v", err)
	}
	return projects, err
}
