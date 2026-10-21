package businness

// "Viewed by" for documents. Recording requires the viewer to be able to access
// the doc; listing viewers is restricted to the owner and editors (viewers and
// commenters cannot see who else has viewed). Delegates to the shared
// ResourceView business once the doc-specific permission check passes.

import (
	"context"
	"errors"
	"time"

	resourceViewBusiness "github.com/akashc777/OneCamp/business/ResourceView"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	auditModels "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
)

// RecordDocView records (deduplicated, throttled) that a user opened the doc.
func RecordDocView(ctx context.Context, docUUID, userUID, userUUID string) error {
	doc, err := domain.GetBasicDgraphDocByUUID(ctx, docUUID, userUID)
	if err != nil {
		return err
	}
	if doc == nil {
		return ErrNotFound
	}
	if IsDeleted(doc) {
		return goneFor(CouldRead(doc, userUUID))
	}
	if !CanRead(doc, userUUID) {
		return errors.New("unauthorized")
	}
	return resourceViewBusiness.RecordView(ctx, resourceViewBusiness.ResourceDoc, docUUID, userUUID)
}

// ListDocViewers returns one page of the doc's distinct viewers (most-recent
// first). Restricted to the owner and editors.
func ListDocViewers(ctx context.Context, docUUID, userUID string, limit, offset int) (*resourceViewBusiness.ViewersPage, error) {
	doc, err := domain.GetBasicDgraphDocByUUID(ctx, docUUID, userUID)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, ErrNotFound
	}
	if IsDeleted(doc) {
		return nil, goneFor(CouldEdit(doc, userUID))
	}
	if !CanEdit(doc, userUID) {
		return nil, errors.New("unauthorized")
	}
	return resourceViewBusiness.ListViewersResolved(ctx, resourceViewBusiness.ResourceDoc, docUUID, limit, offset)
}

// DocEvent is one of the doc's changes as its insights show it: what
// happened (the record's summary), who did it, an agent or the server, when.
type DocEvent struct {
	Action    string    `json:"action"`
	Summary   string    `json:"summary"`
	ActorKind string    `json:"actor_kind,omitempty"` // human, agent or system
	Actor     string    `json:"actor,omitempty"`      // the person's name (for an agent, who asked it)
	At        time.Time `json:"at"`
}

// DocInsightsView is a doc's insights: its last 28 days of views, and its
// newest changes from the record.
type DocInsightsView struct {
	*resourceViewBusiness.Insights
	Events []DocEvent `json:"events"`
}

// docEventsShown is how many of a doc's changes its insights show.
const docEventsShown = 30

// Seams for tests: a doc's rows in the record, and people's names.
var (
	docEvents  = auditModels.ListDocEvents
	namesOf    = userDomain.ResolveUserDisplays
	insightsOf = resourceViewBusiness.InsightsFor
)

// eventsOf are the record's rows as the insights show them, with the names
// of the people who acted. Pure.
func eventsOf(rows []*auditModels.AuditEntry, names map[string]userDomain.UserDisplay) []DocEvent {
	out := make([]DocEvent, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		e := DocEvent{Action: r.Action, Summary: r.Summary, ActorKind: r.ActorKind, At: r.CreatedAt}
		if r.ActorID != nil {
			e.Actor = names[r.ActorID.String()].Display
		}
		out = append(out, e)
	}
	return out
}

// DocInsights is the doc's last 28 days of views, members apart from its
// published page's visitors, and its newest changes from the record. The
// same people as "Viewed by" may read it: the owner and editors. Changes
// that can't be read leave the views to show on their own.
func DocInsights(ctx context.Context, docUUID, userUID string) (*DocInsightsView, error) {
	doc, err := domain.GetBasicDgraphDocByUUID(ctx, docUUID, userUID)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, ErrNotFound
	}
	if IsDeleted(doc) {
		return nil, goneFor(CouldEdit(doc, userUID))
	}
	if !CanEdit(doc, userUID) {
		return nil, errors.New("unauthorized")
	}
	views, err := insightsOf(ctx, resourceViewBusiness.ResourceDoc, docUUID)
	if err != nil {
		return nil, err
	}
	out := &DocInsightsView{Insights: views, Events: []DocEvent{}}
	rows, err := docEvents(ctx, docUUID, docEventsShown)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DocInsights doc %s: its changes couldn't be read: %+v", docUUID, err)
		return out, nil
	}
	ids := []string{}
	for _, r := range rows {
		if r != nil && r.ActorID != nil {
			ids = append(ids, r.ActorID.String())
		}
	}
	names := map[string]userDomain.UserDisplay{}
	if len(ids) > 0 {
		if got, err := namesOf(ctx, ids); err == nil {
			names = got
		}
	}
	out.Events = eventsOf(rows, names)
	return out, nil
}
