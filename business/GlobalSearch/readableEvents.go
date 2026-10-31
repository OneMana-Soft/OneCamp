package business

import (
	"context"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// eventOfHit is the event a hit is; "" for anything else.
func eventOfHit(h *openSearchStruct.GlobalSearchOpenSearchResp) string {
	if h == nil || h.Event == nil {
		return ""
	}
	return h.Event.Uuid
}

// The seams the events' checks read through: which events who may open now
// (the Access Resolver, at view: the calendar's one rule, which an agent's
// bounds narrow too), and the calendars whose events who finds.
var (
	readableEvents = func(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]bool, error) {
		res := make([]accessBusiness.Resource, len(ids))
		for i, id := range ids {
			res[i] = accessBusiness.Resource{Kind: calendarBusiness.KindEvent, UUID: id}
		}
		open, err := accessBusiness.Rules.Filter(ctx, who, res, accessBusiness.LevelView)
		if err != nil {
			return nil, err
		}
		out := make(map[string]bool, len(open))
		for _, r := range open {
			out[strings.ToLower(r.UUID)] = true
		}
		return out, nil
	}
	searchCalendars = calendarBusiness.SearchCalendars
)

// eventCalendars are the calendars whose events who finds (Reach.Calendars).
// A read that fails finds them their own events and their invitations alone.
func eventCalendars(ctx context.Context, who accessBusiness.Subject) *EventCalendars {
	all, open, err := searchCalendars(ctx, who)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "search: could not read the calendars %s sees, so it finds their own events and invitations alone: %v", who.UserUUID, err)
		return &EventCalendars{}
	}
	return &EventCalendars{All: all, Open: open}
}

// keepReadableEventHits returns hits without the events who can't open now:
// in the trash, gone, private to them, or on a calendar they no longer see.
// A check that fails leaves every event out. Hits on anything else are kept
// as they are.
func keepReadableEventHits(ctx context.Context, who accessBusiness.Subject, hits []*openSearchStruct.GlobalSearchOpenSearchResp) []*openSearchStruct.GlobalSearchOpenSearchResp {
	var events []string
	seen := map[string]bool{}
	for _, h := range hits {
		if e := strings.ToLower(eventOfHit(h)); e != "" && !seen[e] {
			seen[e] = true
			events = append(events, e)
		}
	}
	if len(events) == 0 {
		return hits
	}
	readable, err := readableEvents(ctx, who, events)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "search: could not check %d events, leaving their results out: %v", len(events), err)
		readable = nil
	}
	kept := make([]*openSearchStruct.GlobalSearchOpenSearchResp, 0, len(hits))
	dropped := 0
	for _, h := range hits {
		if e := strings.ToLower(eventOfHit(h)); e != "" && !readable[e] {
			dropped++
			continue
		}
		kept = append(kept, h)
	}
	if dropped > 0 {
		helpers.LogInfoWithContext(ctx, "search: left out %d events the person can't open", dropped)
	}
	return kept
}
