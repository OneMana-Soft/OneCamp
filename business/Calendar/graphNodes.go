package Calendar

// The calendar's meetings in the work graph (DG13). A meeting's node is a
// thin projection that business/Refs keeps (domain/WorkGraph nodes.go): who
// organised it and who it's with, the project, channel or team calendar it's
// on, and the docs, tasks and projects its notes link to. Postgres stays the
// source; the node is rebuilt from it.
//
// WHICH EVENTS HAVE ONE. A standalone event or a series that connects
// something beyond its organiser: a member invited who hasn't declined, a
// container's calendar, or a link in its notes. An instance changed on its
// own counts as its series, and an event read from an address, someone's
// own time (ownTimeKinds) and an event of one person alone have none.
//
// HOW IT IS KEPT. Every change the service makes is announced (announce),
// and business/Refs keeps the node from those resource events. An import
// announces nothing, so it tells the graph of its events in one call
// (NodesChanged), and the nightly pass pages through every event that may
// have a node and fixes what those missed. Who may see a meeting is this
// package's rule, decided as the reader by the event reader (access.go).

import (
	"context"
	"strings"

	refsBusiness "github.com/akashc777/OneCamp/business/Refs"
	workGraph "github.com/akashc777/OneCamp/domain/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

func init() {
	// While the calendar isn't read from its store, no event has a node, as
	// its reader decides none (access.go).
	refsBusiness.RegisterProjector(workGraph.KindEvent, refsBusiness.Projector{
		Project: func(ctx context.Context, ids []string) (map[string]workGraph.Projection, error) {
			if !helpers.CalendarReadsFromStore() {
				return map[string]workGraph.Projection{}, nil
			}
			return stored.eventNodes(ctx, ids)
		},
		Page: func(ctx context.Context, after string, n int) ([]string, string, error) {
			if !helpers.CalendarReadsFromStore() {
				return nil, "", nil
			}
			return stored.eventNodesAfter(ctx, after, n)
		},
	})
}

// nodesChanged tells the work graph of the events a writer that announces
// nothing wrote: an import, Google's changes (a seam for tests).
var nodesChanged = refsBusiness.NodesChanged

// maxWith is how many people a meeting's node is with: an all-hands of 200
// reaches each of them, and a larger one its first 200 invited.
const maxWith = 200

// ownTimeKinds are the events that are someone's own time, never a meeting:
// focus, away, where they work, a reminder, and a task block until its
// time_for edge to its task comes (CAL-LEAD-6).
var ownTimeKinds = []string{string(EventFocus), string(EventAway), string(EventWorkingLocation), string(EventReminder), string(EventTaskBlock)}

// containerKinds are the calendars' containers, as the work graph's kinds.
var containerKinds = map[string]workGraph.Kind{
	containerProject: workGraph.KindProject,
	"channel":        workGraph.KindChannel,
	"team":           workGraph.KindTeam,
}

// eventNodes is the projection of each of ids that should have a node: one
// read of the events, one of their guests and one of their calendars.
func (s *service) eventNodes(ctx context.Context, ids []string) (map[string]workGraph.Projection, error) {
	out := map[string]workGraph.Projection{}
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	keys := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if u, err := uuid.Parse(id); err == nil {
			keys = append(keys, u)
		}
	}
	rows, err := s.store.getMany(ctx, keys)
	if err != nil {
		return nil, err
	}
	var events []*models.Event
	var calendars []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, e := range rows {
		if !mayBeMeeting(e) {
			continue
		}
		events = append(events, e)
		if !seen[e.CalendarID] {
			seen[e.CalendarID] = true
			calendars = append(calendars, e.CalendarID)
		}
	}
	if len(events) == 0 {
		return out, nil
	}
	// One more than a node names: the organiser may be among them.
	guests, err := s.store.memberGuests(ctx, idsOf(events), maxWith+1)
	if err != nil {
		return nil, err
	}
	cals, err := s.store.calendarsByID(ctx, calendars)
	if err != nil {
		return nil, err
	}
	on := map[uuid.UUID]workGraph.Ref{}
	for _, c := range cals {
		if k, ok := containerKinds[c.ContainerKind]; ok && helpers.IsCanonicalUUID(c.ContainerID) {
			on[c.ID] = workGraph.Ref{Kind: k, UUID: strings.ToLower(c.ContainerID)}
		}
	}
	for _, e := range events {
		container, hasContainer := on[e.CalendarID]
		var onCalendar *workGraph.Ref
		if hasContainer {
			onCalendar = &container
		}
		if p, ok := meetingNode(e, guests[e.ID], onCalendar); ok {
			out[e.ID.String()] = p
		}
	}
	return out, nil
}

// mayBeMeeting is whether e is the kind of event that has a node when it
// connects something: a series or a single event, not read from an address,
// and not someone's own time. Pure.
func mayBeMeeting(e *models.Event) bool {
	if e.SeriesID != nil || e.Source == sourceSubscribed {
		return false
	}
	for _, k := range ownTimeKinds {
		if e.Kind == k {
			return false
		}
	}
	return true
}

// meetingNode is e's projection, with its member guests in the order they
// were invited and the container whose calendar it's on (nil for none), and
// whether it should have a node: one that connects something beyond its
// organiser. Pure.
func meetingNode(e *models.Event, guests []uuid.UUID, on *workGraph.Ref) (workGraph.Projection, bool) {
	var with []workGraph.Ref
	for _, g := range guests {
		if len(with) == maxWith {
			break
		}
		if g != e.CreatedBy {
			with = append(with, workGraph.Ref{Kind: workGraph.KindPerson, UUID: g.String()})
		}
	}
	links := refsBusiness.BodyLinks(workGraph.KindEvent, e.ID.String(), e.Notes)
	if len(with) == 0 && on == nil && len(links) == 0 {
		return workGraph.Projection{}, false
	}
	p := workGraph.Projection{Changed: e.UpdatedAt, Edges: map[string][]workGraph.Ref{
		"cal_event_organiser": {{Kind: workGraph.KindPerson, UUID: e.CreatedBy.String()}},
		"cal_event_with":      with,
		workGraph.RefersTo:    links,
	}}
	if on != nil {
		p.Edges["cal_event_on"] = []workGraph.Ref{*on}
	}
	if !live(e) {
		p.Deleted = e.UpdatedAt
		if e.DeletedAt != nil {
			p.Deleted = *e.DeletedAt
		}
	}
	return p, true
}

// eventNodesAfter is the nightly pass's page: up to n ids, after the cursor
// (an event's id; "" from the start), of the events that may have a node,
// and the cursor after them ("" at the end).
func (s *service) eventNodesAfter(ctx context.Context, after string, n int) ([]string, string, error) {
	if err := s.ready(ctx); err != nil {
		return nil, "", err
	}
	from, err := uuid.Parse(after)
	if err != nil {
		from = uuid.Nil
	}
	ids, err := s.store.seriesAfter(ctx, from, ownTimeKinds, false, n)
	if err != nil {
		return nil, "", err
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	next := ""
	if len(out) == n && n > 0 {
		next = out[n-1]
	}
	return out, next, nil
}
