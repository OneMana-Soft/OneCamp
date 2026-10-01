package entityLinkBusiness

import (
	"context"

	domain "github.com/akashc777/OneCamp/domain/EntityLink"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Re-export the type constants so callers (controllers) use one vocabulary.
const (
	SourceTask    = domain.SourceTask
	SourceProject = domain.SourceProject
	RefDoc        = domain.RefDoc
	RefBoard      = domain.RefBoard
)

func AddLink(ctx context.Context, sourceType, sourceUUID, refType, refUUID string) error {
	return domain.AddLink(ctx, sourceType, sourceUUID, refType, refUUID)
}

func RemoveLink(ctx context.Context, sourceType, sourceUUID, refType, refUUID string) error {
	return domain.RemoveLink(ctx, sourceType, sourceUUID, refType, refUUID)
}

// CanReadRef reports whether the viewer may see the doc/board being linked or
// inspected (used to gate add and the reverse read).
func CanReadRef(ctx context.Context, refType, refUUID, userDgraphUID, userUUID string) (bool, error) {
	return domain.CanReadRef(ctx, refType, refUUID, userDgraphUID, userUUID)
}

func docVisible(d *dgraphStruct.DgraphDoc, userUUID string) bool {
	if d == nil {
		return false
	}
	if d.IsPrivate == nil || !*d.IsPrivate {
		return true
	}
	if d.HasReadAccess > 0 {
		return true
	}
	return d.CreatedBy != nil && d.CreatedBy.Uuid == userUUID
}

func boardVisible(b *dgraphStruct.DgraphBoard, userUUID string) bool {
	if b == nil {
		return false
	}
	if b.IsPrivate == nil || !*b.IsPrivate {
		return true
	}
	if b.HasReadAccess > 0 {
		return true
	}
	return b.CreatedBy != nil && b.CreatedBy.Uuid == userUUID
}

// GetLinksForSource returns a task/project's linked docs and boards, dropping
// any the viewer can no longer open (private + no access). It also returns
// whether the viewer is authorised (a member), resolved in the same query so
// callers authorise + read in a single round-trip.
func GetLinksForSource(ctx context.Context, sourceType, sourceUUID, userDgraphUID, userUUID string) (docs []*dgraphStruct.DgraphDoc, boards []*dgraphStruct.DgraphBoard, isMember bool, err error) {
	rawDocs, rawBoards, isMember, err := domain.GetLinksForSource(ctx, sourceType, sourceUUID, userDgraphUID)
	if err != nil {
		return nil, nil, false, err
	}
	for _, d := range rawDocs {
		if docVisible(d, userUUID) {
			docs = append(docs, d)
		}
	}
	for _, b := range rawBoards {
		if boardVisible(b, userUUID) {
			boards = append(boards, b)
		}
	}
	return docs, boards, isMember, nil
}

// GetReverseLinksForRef returns the tasks and projects that link a doc/board,
// dropping entries in projects the viewer isn't a member of.
func GetReverseLinksForRef(ctx context.Context, refType, refUUID, userDgraphUID string) (tasks []*dgraphStruct.DgraphTask, projects []*dgraphStruct.DgraphProject, err error) {
	rawTasks, rawProjects, err := domain.GetReverseLinksForRef(ctx, refType, refUUID, userDgraphUID)
	if err != nil {
		return nil, nil, err
	}
	for _, t := range rawTasks {
		if t != nil && t.Project != nil && t.Project.IsProjectMember > 0 {
			tasks = append(tasks, t)
		}
	}
	for _, p := range rawProjects {
		if p != nil && p.IsProjectMember > 0 {
			projects = append(projects, p)
		}
	}
	return tasks, projects, nil
}
