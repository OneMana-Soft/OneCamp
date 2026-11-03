package Calendar

// The calendar in the audit log (calendar A2): every change to an event, from
// its Resource Event, as one row of the hash-chained log an admin reads on
// every plan (Admin › Audit log, "Calendar"). Who made, changed, cancelled,
// deleted, restored or answered which event, and what changed; never its
// title, its notes or a guest's address, which the events never carry (CAL1).
// A bulk write (an import, the demo's seeding) is its job's to record, not
// each event's. Every change to a calendar itself is a row too
// (gcal-new-audit-retention): who shared it with whom at what level, changed
// or stopped a share, renamed or recoloured it, or subscribed to an address
// or dropped one; never its name, and of an address only its host.

import (
	"context"
	"sort"
	"strings"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
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

func init() {
	auditBusiness.RegisterEventSink(auditBusiness.EventSink{
		Name:     "calendar.audit",
		Category: auditBusiness.CategoryCalendar,
		Wants:    auditsEvent,
		Row: func(_ context.Context, e resourceEventBusiness.Event) (string, map[string]interface{}) {
			return auditRow(e)
		},
	})
}

// calendarChanges are the changes to a calendar itself the log records.
var calendarChanges = map[resourceEventBusiness.Kind]bool{
	resourceEventBusiness.CalendarShared:       true,
	resourceEventBusiness.CalendarUnshared:     true,
	resourceEventBusiness.CalendarUpdated:      true,
	resourceEventBusiness.CalendarSubscribed:   true,
	resourceEventBusiness.CalendarUnsubscribed: true,
}

func auditsEvent(e resourceEventBusiness.Event) bool {
	if e.ResourceKind == KindCalendar {
		return calendarChanges[e.Kind]
	}
	_, ok := auditedKinds[e.Kind]
	return ok && e.ResourceKind == KindEvent
}

// auditRow is the row's summary and metadata for e: who did what, and the
// ids and names of what changed, never words from the event. Pure.
func auditRow(e resourceEventBusiness.Event) (string, map[string]interface{}) {
	change := eventChange
	if e.ResourceKind == KindCalendar {
		change = calendarChange
	}
	what, meta := change(e)
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
	return summary, meta
}

// eventChange is what happened to an event, and its row's metadata. Pure.
func eventChange(e resourceEventBusiness.Event) (string, map[string]interface{}) {
	what := auditedKinds[e.Kind]
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
		what += " (" + strings.ReplaceAll(c, ",", ", ") + ")"
	}
	return what, meta
}

// shareWords are a share's levels as the share dialog names them.
var shareWords = map[string]string{models.ShareDetails: "see details", models.ShareEdit: "edit", models.ShareManage: "edit and share"}

// calendarChange is what happened to a calendar, and its row's metadata:
// the calendar's id, the people a share reached, the levels and the host the
// event names. Never the calendar's name, which its events don't carry. Pure.
func calendarChange(e resourceEventBusiness.Event) (string, map[string]interface{}) {
	n, level, from := len(e.Subjects), shareWords[e.Data["level"]], shareWords[e.Data["from"]]
	var what string
	switch e.Kind {
	case resourceEventBusiness.CalendarShared:
		what = "shared a calendar with " + auditBusiness.PeopleWho(n, level)
		if from != "" {
			what = "changed a calendar's share: " + auditBusiness.PeopleWho(n, level) + " now (could " + from + ")"
		}
	case resourceEventBusiness.CalendarUnshared:
		what = "stopped sharing a calendar with " + auditBusiness.People(n)
		if from != "" {
			what += " (could " + from + ")"
		}
	case resourceEventBusiness.CalendarUpdated:
		what = "changed a calendar"
		if c := e.Data["changed"]; c != "" {
			what += " (" + strings.ReplaceAll(strings.ReplaceAll(c, "hue", "colour"), ",", ", ") + ")"
		}
	case resourceEventBusiness.CalendarSubscribed:
		what = "subscribed to a calendar at " + e.Data["host"]
	case resourceEventBusiness.CalendarUnsubscribed:
		what = "unsubscribed from a calendar at " + e.Data["host"]
	}
	meta := auditBusiness.EventMeta(e, true)
	meta["calendar_uuid"] = e.ResourceUUID
	return what, meta
}
