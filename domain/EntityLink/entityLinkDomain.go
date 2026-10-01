package entityLinkDomain

// Entity links: a task or project can link docs and boards (the linked_docs /
// linked_boards edges, with @reverse so a doc/board can surface the tasks and
// projects that reference it). This domain centralises the add / remove / read
// operations for that relationship.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

const (
	SourceTask    = "task"
	SourceProject = "project"
	RefDoc        = "doc"
	RefBoard      = "board"
)

// notDeleted builds this repository's "is not soft-deleted" filter for a Dgraph
// predicate.
//
// IT IS A VALUE TEST, NOT AN EXISTENCE TEST, and that distinction is the whole
// reason this helper exists. Every writer here stores the Go ZERO time in
// *_deleted_at for a live row rather than omitting the predicate, so
// 0001-01-01T00:00:00Z is what "not deleted" looks like on disk and
// has(x_deleted_at) is true for every row that was ever created.
//
// This file used `not has(...)` in all seven of its filters and was the only
// place in the codebase that did: 158 other queries already compared against the
// epoch. The effect was total and silent. The source lookup matched nothing, so
// GetSourceLinks reported the viewer as a non-member and refused every task and
// project regardless of who asked; linked docs, boards, tasks and projects were
// each filtered out of their own results; and AddLink's @if guard never matched,
// so creating a link quietly did nothing. The feature returned an empty,
// authorised-looking answer for everyone.
//
// The epoch rather than the zero time as the boundary, matching the rest: a real
// deletion is stamped with time.Now(), which is comfortably greater than 1970,
// and anything at or below it is a row nobody has deleted.
func notDeleted(predicate string) string {
	return fmt.Sprintf(`not gt(%s, "1970-01-01T00:00:00Z")`, predicate)
}

func sourceUUIDPredicate(sourceType string) (string, bool) {
	switch sourceType {
	case SourceTask:
		return "task_uuid", true
	case SourceProject:
		return "project_uuid", true
	default:
		return "", false
	}
}

func sourceDeletedPredicate(sourceType string) string {
	if sourceType == SourceProject {
		return "project_deleted_at"
	}
	return "task_deleted_at"
}

func refUUIDPredicate(refType string) (string, bool) {
	switch refType {
	case RefDoc:
		return "doc_uuid", true
	case RefBoard:
		return "board_uuid", true
	default:
		return "", false
	}
}

func refDeletedPredicate(refType string) string {
	if refType == RefBoard {
		return "board_deleted_at"
	}
	return "doc_deleted_at"
}

func edgePredicate(refType string) string {
	if refType == RefBoard {
		return "linked_boards"
	}
	return "linked_docs"
}

// mutateLink adds or removes one (source)-(ref) edge. The conditional upsert
// only fires when both nodes exist and are not soft-deleted, so a stale uuid
// is a safe no-op rather than creating a blank node.
func mutateLink(ctx context.Context, sourceType, sourceUUID, refType, refUUID string, remove bool) error {
	srcPred, ok := sourceUUIDPredicate(sourceType)
	if !ok {
		return fmt.Errorf("invalid source type %q", sourceType)
	}
	refPred, ok := refUUIDPredicate(refType)
	if !ok {
		return fmt.Errorf("invalid reference type %q", refType)
	}

	query := fmt.Sprintf(`query {
		src as var(func: eq(%s, %q)) @filter(%s)
		ref as var(func: eq(%s, %q)) @filter(%s)
	}`, srcPred, sourceUUID, notDeleted(sourceDeletedPredicate(sourceType)),
		refPred, refUUID, notDeleted(refDeletedPredicate(refType)))

	edgeJSON := fmt.Sprintf(`{"uid":"uid(src)","%s":[{"uid":"uid(ref)"}]}`, edgePredicate(refType))

	mu := &api.Mutation{Cond: "@if(gt(len(src), 0) AND gt(len(ref), 0))"}
	if remove {
		mu.DeleteJson = []byte(edgeJSON)
	} else {
		mu.SetJson = []byte(edgeJSON)
	}

	txn := dgraphInit.DgraphClient.NewTxn()
	defer func() { _ = txn.Discard(ctx) }()

	_, err := txn.Do(ctx, &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/mutateLink failed (%s->%s remove=%v) err: %+v", sourceType, refType, remove, err)
		return err
	}
	return nil
}

func AddLink(ctx context.Context, sourceType, sourceUUID, refType, refUUID string) error {
	return mutateLink(ctx, sourceType, sourceUUID, refType, refUUID, false)
}

func RemoveLink(ctx context.Context, sourceType, sourceUUID, refType, refUUID string) error {
	return mutateLink(ctx, sourceType, sourceUUID, refType, refUUID, true)
}

type sourceLinksResp struct {
	Q []struct {
		// Project source: membership is on the root.
		IsProjectMember uint8 `json:"project_is_member,omitempty"`
		// Task source: membership is on the task's project.
		TaskProject *struct {
			IsProjectMember uint8 `json:"project_is_member,omitempty"`
		} `json:"task_project,omitempty"`
		LinkedDocs   []*dgraphStruct.DgraphDoc   `json:"linked_docs,omitempty"`
		LinkedBoards []*dgraphStruct.DgraphBoard `json:"linked_boards,omitempty"`
	} `json:"q"`
}

// CountTasksAndLiveTasks returns how many tasks exist and how many the
// soft-delete filter admits.
//
// The health probe's whole question, asked with the SAME notDeleted() the link
// queries use, so a regression in the filter shows up here rather than only in a
// user noticing their links vanished. Counts only; nothing is read or returned.
func CountTasksAndLiveTasks(ctx context.Context) (total, live int, err error) {
	query := fmt.Sprintf(`query {
		total(func: has(task_uuid)) { count(uid) }
		live(func: has(task_uuid)) @filter(%s) { count(uid) }
	}`, notDeleted("task_deleted_at"))

	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	res, qerr := txn.Query(ctx, query)
	if qerr != nil {
		return 0, 0, qerr
	}

	var parsed struct {
		Total []struct {
			Count int `json:"count"`
		} `json:"total"`
		Live []struct {
			Count int `json:"count"`
		} `json:"live"`
	}
	if uerr := json.Unmarshal(res.Json, &parsed); uerr != nil {
		return 0, 0, uerr
	}
	if len(parsed.Total) > 0 {
		total = parsed.Total[0].Count
	}
	if len(parsed.Live) > 0 {
		live = parsed.Live[0].Count
	}
	return total, live, nil
}

// GetLinksForSource returns the docs and boards a task/project links, each
// annotated with the viewer's read access (HasReadAccess) and privacy so the
// caller can hide entries the viewer can no longer open. It also returns
// whether the viewer is a member of the (project of the) source so the caller
// can authorise and read links in a single round-trip instead of two.
type refAccessResp struct {
	Q []struct {
		Private       *bool `json:"private,omitempty"`
		ReadAccess    uint8 `json:"read_access,omitempty"`
		EditAccess    uint8 `json:"edit_access,omitempty"`
		CommentAccess uint8 `json:"comment_access,omitempty"`
		CreatedBy     *struct {
			Uuid string `json:"user_uuid,omitempty"`
		} `json:"created_by,omitempty"`
	} `json:"q"`
}

// CanReadRef reports whether the viewer may see a doc/board: it is public, the
// viewer has read/edit/comment access, or the viewer created it. Used to gate
// linking (you can't attach what you can't see) and the reverse read.
func CanReadRef(ctx context.Context, refType, refUUID, userDgraphUID, userUUID string) (bool, error) {
	refPred, ok := refUUIDPredicate(refType)
	if !ok {
		return false, fmt.Errorf("invalid reference type %q", refType)
	}

	prefix := "doc"
	if refType == RefBoard {
		prefix = "board"
	}

	query := fmt.Sprintf(`query RefAccess($uid: string, $userId: string) {
		q(func: eq(%s, $uid)) @filter(%s) {
			private: %s_private
			read_access: count(%s_reading_users @filter(uid($userId)))
			edit_access: count(%s_editing_users @filter(uid($userId)))
			comment_access: count(%s_commenting_users @filter(uid($userId)))
			created_by: %s_created_by { user_uuid }
		}
	}`, refPred, notDeleted(prefix+"_deleted_at"), prefix, prefix, prefix, prefix, prefix)

	variables := map[string]string{"$uid": refUUID, "$userId": userDgraphUID}

	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	res, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CanReadRef query failed err: %+v", err)
		return false, err
	}

	var parsed refAccessResp
	if err = json.Unmarshal(res.Json, &parsed); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CanReadRef unmarshal failed err: %+v", err)
		return false, err
	}
	if len(parsed.Q) == 0 {
		return false, nil // not found (or deleted)
	}
	row := parsed.Q[0]
	if row.Private == nil || !*row.Private {
		return true, nil
	}
	if row.ReadAccess > 0 || row.EditAccess > 0 || row.CommentAccess > 0 {
		return true, nil
	}
	return row.CreatedBy != nil && row.CreatedBy.Uuid == userUUID, nil
}

func GetLinksForSource(ctx context.Context, sourceType, sourceUUID, userDgraphUID string) (docs []*dgraphStruct.DgraphDoc, boards []*dgraphStruct.DgraphBoard, isMember bool, err error) {
	srcPred, ok := sourceUUIDPredicate(sourceType)
	if !ok {
		return nil, nil, false, fmt.Errorf("invalid source type %q", sourceType)
	}

	// Membership block differs by source: a task inherits its project's
	// membership; a project carries it directly.
	membershipBlock := "project_is_member: count(project_members @filter(uid($userId)))"
	if sourceType == SourceTask {
		membershipBlock = "task_project { project_is_member: count(project_members @filter(uid($userId))) }"
	}

	query := fmt.Sprintf(`query Links($uid: string, $userId: string) {
		q(func: eq(%s, $uid)) @filter(%s) {
			%s
			linked_docs @filter(not gt(doc_deleted_at, "1970-01-01T00:00:00Z")) {
				uid
				doc_uuid
				doc_title
				doc_private
				doc_read_access: count(doc_reading_users @filter(uid($userId)))
				doc_created_by { uid user_uuid user_name user_full_name }
				doc_updated_at
			}
			linked_boards @filter(not gt(board_deleted_at, "1970-01-01T00:00:00Z")) {
				uid
				board_uuid
				board_title
				board_private
				board_thumbnail_key
				board_read_access: count(board_reading_users @filter(uid($userId)))
				board_created_by { uid user_uuid user_name user_full_name }
				board_updated_at
			}
		}
	}`, srcPred, notDeleted(sourceDeletedPredicate(sourceType)), membershipBlock)

	variables := map[string]string{"$uid": sourceUUID, "$userId": userDgraphUID}

	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	res, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetLinksForSource query failed err: %+v", err)
		return nil, nil, false, err
	}

	var parsed sourceLinksResp
	if err = json.Unmarshal(res.Json, &parsed); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetLinksForSource unmarshal failed err: %+v", err)
		return nil, nil, false, err
	}
	if len(parsed.Q) == 0 {
		return nil, nil, false, nil
	}
	root := parsed.Q[0]
	if sourceType == SourceTask {
		isMember = root.TaskProject != nil && root.TaskProject.IsProjectMember > 0
	} else {
		isMember = root.IsProjectMember > 0
	}
	return root.LinkedDocs, root.LinkedBoards, isMember, nil
}

type refLinksResp struct {
	Q []struct {
		LinkedTasks    []*dgraphStruct.DgraphTask    `json:"linked_tasks,omitempty"`
		LinkedProjects []*dgraphStruct.DgraphProject `json:"linked_projects,omitempty"`
	} `json:"q"`
}

// GetReverseLinksForRef returns the tasks and projects that link a given
// doc/board, annotated with the viewer's project membership so the caller can
// hide entries in projects the viewer isn't part of.
func GetReverseLinksForRef(ctx context.Context, refType, refUUID, userDgraphUID string) (tasks []*dgraphStruct.DgraphTask, projects []*dgraphStruct.DgraphProject, err error) {
	refPred, ok := refUUIDPredicate(refType)
	if !ok {
		return nil, nil, fmt.Errorf("invalid reference type %q", refType)
	}
	reverseEdge := "~" + edgePredicate(refType)

	query := fmt.Sprintf(`query ReverseLinks($uid: string, $userId: string) {
		q(func: eq(%s, $uid)) @filter(%s) {
			linked_tasks: %s @filter(type(Task) AND not gt(task_deleted_at, "1970-01-01T00:00:00Z")) {
				uid
				task_uuid
				task_name
				task_status
				task_custom_status
				task_custom_status_name
				task_project {
					uid
					project_uuid
					project_name
					project_is_member: count(project_members @filter(uid($userId)))
				}
			}
			linked_projects: %s @filter(type(Project) AND not gt(project_deleted_at, "1970-01-01T00:00:00Z")) {
				uid
				project_uuid
				project_name
				project_status
				project_is_member: count(project_members @filter(uid($userId)))
			}
		}
	}`, refPred, notDeleted(refDeletedPredicate(refType)), reverseEdge, reverseEdge)

	variables := map[string]string{"$uid": refUUID, "$userId": userDgraphUID}

	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	res, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetReverseLinksForRef query failed err: %+v", err)
		return nil, nil, err
	}

	var parsed refLinksResp
	if err = json.Unmarshal(res.Json, &parsed); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetReverseLinksForRef unmarshal failed err: %+v", err)
		return nil, nil, err
	}
	if len(parsed.Q) == 0 {
		return nil, nil, nil
	}
	return parsed.Q[0].LinkedTasks, parsed.Q[0].LinkedProjects, nil
}
