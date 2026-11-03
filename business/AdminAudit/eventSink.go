package business

// Resource Events in the admin audit log: one sink, shared by every area.
//
// Docs (business/Follow), the calendar (business/Calendar) and boards
// (business/Board) each record their changes here in their own words. An area
// says which events it records, the log's area its rows go under, and how a
// row reads. This file does the rest, the same way for all of them:
//
//   - who acted: a person; an agent (the row names the person who asked it,
//     says an agent acted, and its action is agent.<kind>, so the AI activity
//     feed lists it beside the agent's other work); or OneCamp itself, for a
//     sweep;
//   - the write into the hash chain;
//   - the log line when a row can't be written.
//
// A row is written while the sink runs, after the change: a change is never
// held up, or refused, for its record.

import (
	"context"
	"fmt"
	"strings"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// EventSink is one area's Resource Events in the log.
type EventSink struct {
	// Name is the sink's name on the event bus, unique there.
	Name string
	// Category is the log's area the rows go under (CategoryDocs, ...).
	Category string
	// Wants says which events the area records.
	Wants func(resourceEventBusiness.Event) bool
	// Row is the summary and metadata of a wanted event's row.
	Row func(ctx context.Context, e resourceEventBusiness.Event) (string, map[string]interface{})
	// InBulk records a bulk import's events too. Without it, a bulk write (an
	// import, the demo's seeding) is its job's to record, not each item's.
	InBulk bool
}

// RegisterEventSink puts an area's sink on the event bus. Call it from the
// area's package init; a sink missing its category, its filter or its row
// panics there, so the build's tests fail instead of events going unrecorded.
func RegisterEventSink(s EventSink) {
	if s.Category == "" || s.Wants == nil || s.Row == nil {
		panic(fmt.Sprintf("admin audit: the event sink %q needs a category, the events it wants and how a row reads", s.Name))
	}
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: s.Name, Wants: s.Wants, Run: s.Run, InBulk: s.InBulk})
}

// Run writes e's row: the area's words, under the area's category.
func (s EventSink) Run(ctx context.Context, e resourceEventBusiness.Event) {
	summary, meta := s.Row(ctx, e)
	kind := ActorHuman
	switch {
	case e.Actor.AgentUUID != "":
		kind = ActorAgent
	case e.Actor.System || e.Actor.UserUUID == "":
		kind = ActorSystem
	}
	var actor *uuid.UUID
	email := ""
	if id, err := uuid.Parse(e.Actor.UserUUID); err == nil {
		actor = &id
		email = EventActorEmail(ctx, id)
	}
	action := string(e.Kind)
	if e.Actor.AgentUUID != "" {
		action = "agent." + action
	}
	if err := RecordEventRow(ctx, actor, email, kind, action, s.Category, summary, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "admin audit: %s on %s %s wasn't recorded (sink %s): %+v", e.Kind, e.ResourceKind, e.ResourceUUID, s.Name, err)
	}
}

// Seams for the areas' tests: where an event's row goes, and an actor's
// address.
var (
	RecordEventRow  = RecordForPrincipal
	EventActorEmail = func(ctx context.Context, id uuid.UUID) string {
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

// People is how many people a row names: "1 person", "2 people".
func People(n int) string {
	if n == 1 {
		return "1 person"
	}
	return fmt.Sprintf("%d people", n)
}

// PeopleWho is the people a share reached and what they can do now: "1 person
// who can edit", "2 people who have full access".
func PeopleWho(n int, level string) string {
	have := "has"
	if n != 1 {
		have = "have"
	}
	if level == "full" {
		return People(n) + " who " + have + " full access"
	}
	return People(n) + " who can " + level
}

// EventMeta is an event's row metadata, as the areas write it: the resource,
// when it happened, the agent when one acted, the people it reached, and the
// event's data, with a private resource's title left out (admins can't open
// it, so the log never names it). Pure.
func EventMeta(e resourceEventBusiness.Event, private bool) map[string]interface{} {
	meta := map[string]interface{}{
		"resource_kind": e.ResourceKind,
		"resource_uuid": e.ResourceUUID,
		"at":            e.At.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	}
	if e.Actor.AgentUUID != "" {
		meta["agent_uuid"] = e.Actor.AgentUUID
	}
	if len(e.Subjects) > 0 {
		meta["people"] = strings.Join(e.Subjects, ",")
	}
	for k, v := range e.Data {
		if k == "title" && private {
			continue
		}
		meta[k] = v
	}
	return meta
}
