package business

// A client's verdict as an approval's decision (package 9 R2b with
// basecamp-25's H-5; agreed with PROJ-LEAD-4 and DOCS-LEAD-2). A client
// answers through a link, and the answer counts on the item's open request
// only when the request names that link (client_grants); otherwise it stays
// the client_reviews row it always was. A guest is never a user: the
// decision is the link's, under the name the client gave. An approval that
// completes a held move makes it through finish, so StatusGate sees it
// approved, as a member's would. The link's own page says what waits for
// the client's yes (clientAsks).

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
)

// restrictionOf is a task's restriction (WA2). A seam for tests.
var restrictionOf = restrictionModel.Of

func init() {
	guestBusiness.ClientVerdict = clientVerdict
	guestBusiness.ClientAsks = clientAsks
}

// ErrPrivateTask is a task kept to some members (WA2), which no client link
// sees, so no client can approve it.
var ErrPrivateTask = errors.New("this task is private to some members, so the client can't see it")

// clientLinks is the client links that can see the task and comment, so can
// answer it: a project's live links (ListActiveForResource) that can
// comment, and none for a task kept to some members (ErrPrivateTask). It
// takes the task so "Client sees this?" (basecamp-24's H-4) can decide per
// item here, with no change to its callers. A seam for tests.
var clientLinks = func(ctx context.Context, projectID, taskID uuid.UUID) ([]string, error) {
	r, err := restrictionOf(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if r.Restricted {
		return nil, ErrPrivateTask
	}
	grants, err := guestModel.ListActiveForResource(ctx, guestModel.ResourceProject, projectID.String())
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, g := range grants {
		if g.Capability == guestModel.CapabilityComment {
			out = append(out, g.Id.String())
		}
	}
	return out, nil
}

// Seams for tests: an item's open request, a link's answer, a move's status name.
var (
	pendingForItem = model.PendingForItem
	decideAs       = model.DecideAs
	statusName     = func(ctx context.Context, projectID uuid.UUID, status string) string {
		if r, err := taskStatusBusiness.Resolve(ctx, projectID.String(), status); err == nil {
			return r.Display()
		}
		return status
	}
)

// clientVerdict counts a client's verdict ("approved" or "changes", the
// reviews' words) on the item's open request, when the request names the
// link it came through.
func clientVerdict(ctx context.Context, grant uuid.UUID, kind, id, title, decision, name, note string) {
	verdict := map[string]string{"approved": model.VerdictApprove, "changes": model.VerdictChanges}[decision]
	if verdict == "" || grant == uuid.Nil {
		return
	}
	a, err := pendingForItem(ctx, kind, id)
	if err != nil {
		if !errors.Is(err, model.ErrNotFound) {
			helpers.LogErrorWithContext(ctx, "Approvals: a client's verdict on %s %s: %v", kind, id, err)
		}
		return
	}
	if !slices.Contains(a.ClientGrants, grant.String()) {
		return
	}
	if name == "" {
		name = "The client"
	}
	decided, err := decideAs(ctx, a.ID, model.Decider{Grant: grant, Name: name}, verdict, note)
	if err != nil {
		if !errors.Is(err, model.ErrNotFound) {
			helpers.LogErrorWithContext(ctx, "Approvals: a client's decision on %s: %v", a.ID, err)
		}
		return
	}
	a = decided
	toName := ""
	if a.ToStatus != "" {
		toName = statusName(ctx, a.ProjectID, a.ToStatus)
	}
	settle(ctx, a, &dgraphStruct.DgraphTask{Uuid: id, Name: title}, nil, name+" (client)", verdict, toName)
}

// Seams for tests: a link's open requests, and a project's status names.
var (
	forLink     = model.ForLink
	statusNames = taskStatusBusiness.Names
)

// clientAsks is what a link's client is asked to approve in a project, by
// task: each open request that names the link and that no client has
// answered, where a held move goes (by the project's name for the status),
// and who asked.
func clientAsks(ctx context.Context, grant, projectID uuid.UUID) (map[string]guestBusiness.ClientAsk, error) {
	open, err := forLink(ctx, projectID, grant)
	if err != nil || len(open) == 0 {
		return nil, err
	}
	nameOf, err := statusNames(ctx, projectID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(open))
	for _, a := range open {
		ids = append(ids, a.RequestedBy.String())
	}
	askers := whoAsked(ctx, ids)
	asks := make(map[string]guestBusiness.ClientAsk, len(open))
	for _, a := range open {
		ask := guestBusiness.ClientAsk{Ask: a.Ask, DueAt: a.DueAt, AskedBy: askers[a.RequestedBy.String()], AskedAt: a.CreatedAt}
		if a.ToStatus != "" {
			ask.ToName = nameOf(a.ToStatus)
		}
		asks[a.ResourceID] = ask
	}
	return asks, nil
}
