package business

// The doc.moved rule (notify.go): a page moved into another space, or out of
// one, by someone other than its owner tells the owner. Everyone in a
// project's or a team's space can open every page in it (business/Access
// space.go), so the move shared their page with a space's people, or took it
// from them. doc.moved says so and names the owner (business/Doc EmitMoved);
// a move that keeps the page's space names nobody, and tells nobody.

import (
	"context"
	"strings"
	"time"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Seam for tests: a space's name ("" when it can't be read).
var spaceName = func(ctx context.Context, kind, spaceUUID string) string {
	name, _, err := docDomain.GetSpace(ctx, kind, spaceUUID)
	if err != nil {
		return ""
	}
	return name
}

// tellsOfSpaceMove is a doc.moved the rule tells anyone about: one that
// changed the page's space and names someone (its owner) to tell.
func tellsOfSpaceMove(e resourceEventBusiness.Event) bool {
	return e.Data["space"] != "" && len(e.Subjects) > 0
}

// tellSpaceMoved tells the people the event names (the page's owner), the
// ones who may open the page now, that it moved into a space or out of one.
func tellSpaceMoved(ctx context.Context, e resourceEventBusiness.Event, doc *dgraphStruct.DgraphDoc, c cast) {
	readers, ok := readersOf(ctx, e, doc, e.Subjects)
	if !ok {
		return
	}
	var to []notificationBusiness.NoticeRecipient
	for _, id := range e.Subjects {
		if readers[id] {
			to = append(to, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: "owner"})
		}
	}
	if len(to) == 0 {
		return
	}
	n := baseNotice(e, doc, c)
	n.Data = map[string]string{"space": e.Data["space"], "was": e.Data["was"]}
	n.DedupKey = string(e.Kind) + "|" + doc.Uuid + "|" + e.At.UTC().Format(time.RFC3339Nano)
	if e.Data["space"] != "none" {
		name, named := spaceCalled(ctx, e.Data["space"])
		first, again := name, name
		if !named {
			first, again = "a "+name, "that "+name
		}
		n.Title = c.who() + " moved ‘" + titleOf(doc) + "’ into " + first + c.forWhom()
		n.Body = "Everyone in " + again + " can open it now."
	} else {
		name, named := spaceCalled(ctx, e.Data["was"])
		first, again := name, name
		if !named {
			first, again = "its "+name, "that "+name
		}
		n.Title = c.who() + " moved ‘" + titleOf(doc) + "’ out of " + first + c.forWhom()
		n.Body = "Being in " + again + " no longer lets people open it."
	}
	tell(ctx, n, to)
}

// spaceCalled is a space ("project:<uuid>", "team:<uuid>") as a notice names
// it: its name (named), or when that can't be read, what kind it is.
func spaceCalled(ctx context.Context, ref string) (name string, named bool) {
	_, kind, id := docBusiness.TreeSection(ref)
	if name := strings.TrimSpace(spaceName(ctx, kind, id)); name != "" {
		return name, true
	}
	if kind != "" {
		return kind, false
	}
	return "space", false
}
