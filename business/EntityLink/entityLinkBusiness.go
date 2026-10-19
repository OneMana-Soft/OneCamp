package entityLinkBusiness

import (
	"context"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"

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
// inspected (used to gate add and the reverse read). A doc is named to them
// only when they may open it (business/Access ReadableTitles, read as its
// chip is, by the Access Resolver's rule); a board, by its own rule.
func CanReadRef(ctx context.Context, refType, refUUID, userDgraphUID, userUUID string) (bool, error) {
	if refType != RefDoc {
		return domain.CanReadRef(ctx, refType, refUUID, userDgraphUID, userUUID)
	}
	ref := accessBusiness.ItemRef{Kind: accessBusiness.KindDoc, ID: refUUID}
	named, err := accessBusiness.ReadableTitles(ctx, []accessBusiness.Subject{{UserUID: userDgraphUID, UserUUID: userUUID}}, []accessBusiness.ItemRef{ref})
	if err != nil {
		return false, err
	}
	_, ok := named[ref]
	return ok, nil
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
	// Which linked docs the viewer may open: the Access Resolver, in one read.
	res := make([]accessBusiness.Resource, 0, len(rawDocs))
	for _, d := range rawDocs {
		if d != nil && d.Uuid != "" {
			res = append(res, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: d.Uuid})
		}
	}
	open, err := accessBusiness.Rules.Filter(ctx, accessBusiness.Subject{UserUID: userDgraphUID, UserUUID: userUUID}, res, accessBusiness.LevelView)
	if err != nil {
		return nil, nil, false, err
	}
	readable := make(map[string]bool, len(open))
	for _, r := range open {
		readable[r.UUID] = true
	}
	for _, d := range rawDocs {
		if d != nil && readable[d.Uuid] {
			// The app marks a private link it can't open by this count:
			// every link kept here opens.
			d.HasReadAccess = 1
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
// dropping entries in projects the viewer isn't in (a member or an admin).
func GetReverseLinksForRef(ctx context.Context, refType, refUUID, userDgraphUID string) (tasks []*dgraphStruct.DgraphTask, projects []*dgraphStruct.DgraphProject, err error) {
	rawTasks, rawProjects, err := domain.GetReverseLinksForRef(ctx, refType, refUUID, userDgraphUID)
	if err != nil {
		return nil, nil, err
	}
	for _, t := range rawTasks {
		if t != nil && t.Project.ReaderIsIn() {
			tasks = append(tasks, t)
		}
	}
	for _, p := range rawProjects {
		if p.ReaderIsIn() {
			projects = append(projects, p)
		}
	}
	return tasks, projects, nil
}
