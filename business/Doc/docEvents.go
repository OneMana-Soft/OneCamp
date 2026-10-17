package businness

// The Resource Events of a doc's own changes (I14): a rename, a change to who
// may open it or comment on it, and sharing it. The bus tells whoever each is
// for (business/Follow): open tabs hear of a rename or an access change at once,
// the people a doc is shared with are told, and the audit records sharing and
// access. A save of its body is doc.updated, emitted once by the save itself
// (docCollabSave.go savedEvent, which credits an agent and who asked it), and
// webhooks gather those into one a minute per doc.
//
// An event is emitted only for what changed (I15): a rename to the same title,
// or adding someone who could already open the doc, emits nothing.

import (
	"context"
	"strconv"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/User"
)

// Seam for tests: the doc as a rename or privacy change finds it.
var docHead = domain.GetDocHead

// actorFrom is who is making a request's change (I11): the signed-in person, or
// the server itself when no one is (a sweep, the demo's seed).
func actorFrom(ctx context.Context) resourceEventBusiness.Actor {
	if u, ok := model.FromContext(ctx); ok && u.UserDgraphInfo.Uuid != "" {
		return resourceEventBusiness.Actor{UserUUID: u.UserDgraphInfo.Uuid}
	}
	return resourceEventBusiness.Actor{System: true}
}

// saveActor is who a save from the collaboration service is by, as its
// doc.mentioned names them (docMentions.go): the one person whose edits it
// holds, or with several, nobody in particular (Data lists them).
func saveActor(contributors []string) (resourceEventBusiness.Actor, map[string]string) {
	switch len(contributors) {
	case 0:
		return resourceEventBusiness.Actor{}, nil
	case 1:
		return resourceEventBusiness.Actor{UserUUID: contributors[0]}, nil
	}
	return resourceEventBusiness.Actor{}, map[string]string{"contributors": strings.Join(contributors, ",")}
}

// headBeforeUpdate reads the doc before an update that renames it or changes
// who may open or comment on it; nil for a body save, which asks nothing.
func headBeforeUpdate(ctx context.Context, in *adapter.InputUpdateDoc) *dgraphStruct.DgraphDoc {
	if in.Title == nil && in.IsPrivate == nil && in.PublicComment == nil {
		return nil
	}
	head, err := docHead(ctx, in.DocId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/headBeforeUpdate doc %s err: %+v", in.DocId, err)
		return nil
	}
	return head
}

// emitUpdated tells the bus what an update changed, against the doc as it was
// before (headBeforeUpdate). Nothing for a body save.
func emitUpdated(ctx context.Context, before *dgraphStruct.DgraphDoc, in *adapter.InputUpdateDoc) {
	if before == nil {
		return
	}
	for _, e := range updateEvents(before, in) {
		e.Actor, e.At = actorFrom(ctx), time.Now()
		resourceEventBusiness.Bus.Emit(ctx, e)
	}
}

// updateEvents are the events an update makes of a doc read before it. Pure.
func updateEvents(before *dgraphStruct.DgraphDoc, in *adapter.InputUpdateDoc) []resourceEventBusiness.Event {
	var out []resourceEventBusiness.Event
	ev := func(kind resourceEventBusiness.Kind, data map[string]string) {
		out = append(out, resourceEventBusiness.Event{Kind: kind, ResourceKind: "doc", ResourceUUID: in.DocId, Data: data})
	}
	if in.Title != nil && *in.Title != before.Title {
		ev(resourceEventBusiness.DocRenamed, map[string]string{"title": *in.Title})
	}
	access := map[string]string{}
	wasPrivate := before.IsPrivate != nil && *before.IsPrivate
	if in.IsPrivate != nil && *in.IsPrivate != wasPrivate {
		access["private"] = strconv.FormatBool(*in.IsPrivate)
	}
	wasOpen := before.PublicComment != nil && *before.PublicComment
	if in.PublicComment != nil && *in.PublicComment != wasOpen {
		access["public_comment"] = strconv.FormatBool(*in.PublicComment)
	}
	if len(access) > 0 {
		ev(resourceEventBusiness.DocAccessChanged, access)
	}
	return out
}

// emitShared tells the bus what a change to a doc's sharing did, against the
// sharing read before it (nil if it couldn't be read: then nobody is told,
// rather than everyone named told twice). input's lists are as the caller sent
// them, before the single-role rule added to its removals.
func emitShared(ctx context.Context, before *dgraphStruct.DgraphDoc, input adapter.InputUpdateDocPermissions, owner string) {
	if before == nil {
		return
	}
	actor := actorFrom(ctx)
	if actor.System && owner != "" {
		actor = resourceEventBusiness.Actor{UserUUID: owner}
	}
	for _, e := range sharingEvents(before, input, owner) {
		e.Actor, e.At = actor, time.Now()
		resourceEventBusiness.Bus.Emit(ctx, e)
	}
}

// sharingEvents are the events of a sharing change: doc.shared, one for each
// level, naming the people who couldn't open the doc before and can now; and
// doc.access_changed naming those whose level changed or who lost it. Pure.
func sharingEvents(before *dgraphStruct.DgraphDoc, input adapter.InputUpdateDocPermissions, owner string) []resourceEventBusiness.Event {
	had := map[string]string{}
	for level, list := range map[string][]*dgraphStruct.DgraphUser{"edit": before.EditingUser, "comment": before.CommentingUser, "view": before.ReadingUser} {
		for _, u := range list {
			if u != nil && u.Uuid != "" {
				had[u.Uuid] = level
			}
		}
	}
	added := map[string]bool{}
	gained := map[string][]string{}
	var changed []string
	for _, add := range []struct {
		level string
		ids   []string
	}{{"edit", input.AddEditors}, {"comment", input.AddCommenters}, {"view", input.AddViewers}} {
		for _, id := range add.ids {
			if id == "" || id == owner || added[id] {
				continue
			}
			added[id] = true
			switch was, ok := had[id]; {
			case !ok:
				gained[add.level] = append(gained[add.level], id)
			case was != add.level:
				changed = append(changed, id)
			}
		}
	}
	for _, list := range [][]string{input.RemoveEditors, input.RemoveCommenters, input.RemoveViewers} {
		for _, id := range list {
			if _, ok := had[id]; ok && !added[id] {
				added[id] = true
				changed = append(changed, id)
			}
		}
	}
	var out []resourceEventBusiness.Event
	for _, level := range []string{"edit", "comment", "view"} {
		if len(gained[level]) > 0 {
			out = append(out, resourceEventBusiness.Event{Kind: resourceEventBusiness.DocShared, ResourceKind: "doc", ResourceUUID: input.DocId,
				Subjects: gained[level], Data: map[string]string{"level": level}})
		}
	}
	if len(changed) > 0 {
		out = append(out, resourceEventBusiness.Event{Kind: resourceEventBusiness.DocAccessChanged, ResourceKind: "doc", ResourceUUID: input.DocId,
			Subjects: changed})
	}
	return out
}
