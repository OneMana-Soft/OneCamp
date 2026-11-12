package entityLinkBusiness

import (
	"context"
	"fmt"
	"strings"

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
	RefTask       = domain.RefTask
)

func AddLink(ctx context.Context, sourceType, sourceUUID, refType, refUUID string) error {
	return domain.AddLink(ctx, sourceType, sourceUUID, refType, refUUID)
}

func RemoveLink(ctx context.Context, sourceType, sourceUUID, refType, refUUID string) error {
	return domain.RemoveLink(ctx, sourceType, sourceUUID, refType, refUUID)
}

// CanReadRef reports whether the viewer may open the doc, board or task
// being linked or inspected (used to gate add and the reverse read): ReadableRefs
// for one.
func CanReadRef(ctx context.Context, refType, refUUID, userDgraphUID, userUUID string) (bool, error) {
	ref := Ref{Kind: refType, ID: refUUID}
	open, err := ReadableRefs(ctx, accessBusiness.Subject{UserUID: userDgraphUID, UserUUID: userUUID}, []Ref{ref})
	if err != nil {
		return false, err
	}
	return open[ref], nil
}

// ReadableRefs is which of refs (a doc, a board or a task each) who may open
// now, keyed as asked: the rule that gates every link to one, and every read
// of a link. A doc and a task are named to them only when they may open it,
// read as its chip is (business/Access ReadableTitles: the Access Resolver's
// rule for a doc, the task view's for a task, readTasks), one read per kind
// however many are asked; boards by their own rule, in one read; any other
// kind is an error. Any error is the whole answer.
func ReadableRefs(ctx context.Context, who accessBusiness.Subject, refs []Ref) (map[Ref]bool, error) {
	out := make(map[Ref]bool, len(refs))
	var titled []accessBusiness.ItemRef
	var boards []string
	for _, r := range refs {
		switch r.Kind {
		case RefDoc, RefTask:
			titled = append(titled, accessBusiness.ItemRef{Kind: r.Kind, ID: r.ID})
		case RefBoard:
			boards = append(boards, r.ID)
		default:
			return nil, fmt.Errorf("invalid reference type %q", r.Kind)
		}
	}
	if len(boards) > 0 {
		open, err := domain.CanReadRefs(ctx, RefBoard, boards, who.UserUID, who.UserUUID)
		if err != nil {
			return nil, err
		}
		for _, id := range boards {
			if open[strings.ToLower(id)] {
				out[Ref{Kind: RefBoard, ID: id}] = true
			}
		}
	}
	if len(titled) == 0 {
		return out, nil
	}
	named, err := accessBusiness.ReadableTitles(ctx, []accessBusiness.Subject{who}, titled)
	if err != nil {
		return nil, err
	}
	for ref := range named {
		out[Ref{Kind: ref.Kind, ID: ref.ID}] = true
	}
	return out, nil
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
