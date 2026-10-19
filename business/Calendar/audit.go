package Calendar

// The calendar in the audit log (calendar A2): every change to an event, from
// its Resource Event, as one row of the hash-chained log an admin reads on
// every plan (Admin › Audit log, "Calendar"). Who made, changed, cancelled,
// deleted, restored or answered which event, and what changed; never its
// title, its notes or a guest's address, which the events never carry (CAL1).
// A bulk write (an import, the demo's seeding) is its job's to record, not
// each event's.

import (
	"context"
	"sort"
	"strings"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// auditedKinds are the calendar's events the audit log records, with what
// each row says happened.
var auditedKinds = map[resourceEventBusiness.Kind]string{
	resourceEventBusiness.CalendarEventCreated:   "made an event",
	resourceEventBusiness.CalendarEventUpdated:   "changed an event",
	resourceEventBusiness.CalendarEventCancelled: "cancelled part of a repeating event",
	resourceEventBusiness.CalendarEventDeleted:   "deleted an event",
	resourceEventBusiness.CalendarEventRestored:  "restored an event",
	resourceEventBusiness.CalendarEventRSVP:      "answered an invitation",
}

// Seams for tests: where a row goes, and an actor's address.
var (
	calendarAudit = auditBusiness.RecordForPrincipal
	auditEmail    = func(ctx context.Context, id uuid.UUID) string {
		if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
			return ""
		}
		u, err := userDomain.GetUserByUUID(ctx, id)
		if err != nil || u == nil {
			return ""
		}
		return u.EmailID
	}
)

func init() {
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "calendar.audit", Wants: auditsEvent, Run: auditEvent})
}

func auditsEvent(e resourceEventBusiness.Event) bool {
	_, ok := auditedKinds[e.Kind]
	return ok && e.ResourceKind == KindEvent
}

// auditRow is the row's summary and metadata for e: who did what, and the
// ids and names of what changed, never words from the event. Pure.
func auditRow(e resourceEventBusiness.Event) (string, map[string]interface{}) {
	what := auditedKinds[e.Kind]
	// The row's actor says who a person is; the summary says what they did.
	summary := strings.ToUpper(what[:1]) + what[1:]
	switch {
	case e.Actor.AgentUUID != "":
		summary = "An agent " + what
	case e.Actor.UserUUID == "" && e.Data["by"] == "guest":
		summary = "A guest invited by email " + what
	case e.Actor.System || e.Actor.UserUUID == "":
		summary = "OneCamp " + what
	}
	meta := map[string]interface{}{"event_uuid": e.ResourceUUID}
	keys := make([]string, 0, len(e.Data))
	for k := range e.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "kind", "changed", "scope", "instance", "response", "from", "by":
			meta[k] = e.Data[k]
		}
	}
	if c := e.Data["changed"]; c != "" {
		summary += " (" + strings.ReplaceAll(c, ",", ", ") + ")"
	}
	return summary, meta
}

func auditEvent(ctx context.Context, e resourceEventBusiness.Event) {
	summary, meta := auditRow(e)
	kind := auditBusiness.ActorHuman
	switch {
	case e.Actor.AgentUUID != "":
		kind = auditBusiness.ActorAgent
	case e.Actor.System || e.Actor.UserUUID == "":
		kind = auditBusiness.ActorSystem
	}
	var actor *uuid.UUID
	email := ""
	if id, err := uuid.Parse(e.Actor.UserUUID); err == nil {
		actor = &id
		email = auditEmail(ctx, id)
	}
	action := string(e.Kind)
	if e.Actor.AgentUUID != "" {
		action = "agent." + action
	}
	if err := calendarAudit(ctx, actor, email, kind, action, auditBusiness.CategoryCalendar, summary, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar audit: %s on event %s wasn't recorded: %+v", e.Kind, e.ResourceUUID, err)
	}
}
