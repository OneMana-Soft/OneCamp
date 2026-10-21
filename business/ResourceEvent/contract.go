package business

// Resource Events: one Emit for every server-side change to a resource (docs
// programme, PROGRAM.md §4 #13 and invariant I14). The bus fans each event out
// once to the sinks that registered for it (sinks.go): the notifications (to the
// people the event is for, deduplicated), the audit log, MQTT meta events for
// open tabs, webhooks, the inbox and, in the AI edition, the AI record.
//
// Emitting is one call, from the code that made the change, after the change is
// stored:
//
//	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
//		Kind: resourceEventBusiness.DocRenamed, ResourceKind: "doc", ResourceUUID: id,
//		Actor: resourceEventBusiness.Actor{UserUUID: me}, At: time.Now(),
//		Data: map[string]string{"title": title},
//	})
//
// Emit returns at once and never fails the change: every sink runs on its own,
// after the caller has moved on, a few of its events at a time, and a sink that
// fails logs it. Delivery is at most once: there is no outbox, so an event a
// sink failed on, or one still waiting when the process stopped, is not tried
// again. A test that wants to see what was emitted swaps Bus for a recorder and
// puts it back.

import (
	"context"
	"time"
)

// Kind is what happened. A kind is never renamed once shipped: webhooks and
// audit rows carry it.
type Kind string

const (
	DocCreated        Kind = "doc.created"
	DocUpdated        Kind = "doc.updated" // the body changed
	DocRenamed        Kind = "doc.renamed"
	DocMoved          Kind = "doc.moved"
	DocDeleted        Kind = "doc.deleted" // to the trash
	DocRestored       Kind = "doc.restored"
	DocPurged         Kind = "doc.purged"
	DocArchived       Kind = "doc.archived"
	DocShared         Kind = "doc.shared" // someone gained access
	DocAccessChanged  Kind = "doc.access_changed"
	DocLocked         Kind = "doc.locked"
	DocRestyled       Kind = "doc.restyled" // its icon, cover, width, font or text size
	DocVersionRestore Kind = "doc.version_restored"
	DocExported       Kind = "doc.exported"
	DocPublished      Kind = "doc.published"
	DocTransferred    Kind = "doc.transferred"
	DocRecovered      Kind = "doc.recovered" // an admin recovered it
	DocMentioned      Kind = "doc.mentioned"
	DocCommentCreated Kind = "doc.comment.created"

	// Edit proposals (business/Proposal), named after the resource's kind: a
	// proposal on a doc is "doc.proposal.created". Subjects, on a decision, is
	// the proposer, or the person who asked the agent that proposed it.
	DocProposalCreated   Kind = "doc.proposal.created"
	DocProposalAccepted  Kind = "doc.proposal.accepted"
	DocProposalDeclined  Kind = "doc.proposal.declined"
	DocProposalWithdrawn Kind = "doc.proposal.withdrawn"

	// Asking for access (business/Access requests.go). Subjects are, on a
	// request, the people who can answer it (the doc's owner and its
	// full-access people), and on an answer, the person who asked. Data:
	// request (its id) and level (asked for, or on an approval, given), and on
	// an answer its status, approved or declined.
	DocAccessRequested Kind = "doc.access_requested"
	DocAccessAnswered  Kind = "doc.access_answered"

	// Anchored threads (business/Thread), on the resource the thread is on.
	// Data: thread_id, anchor_kind, status, and comment_id for created and
	// replied. Subjects are the thread's participants and anyone newly
	// mentioned, never the actor.
	ThreadCreated    Kind = "thread.created"
	ThreadReplied    Kind = "thread.replied"
	ThreadResolved   Kind = "thread.resolved"
	ThreadReopened   Kind = "thread.reopened"
	ThreadReanchored Kind = "thread.reanchored"

	// Chat (design/chat-vs-slack/PROGRAM.md §5), declared at its day 0 and
	// emitted by the packages that build them (A2 messages, A9 conversations and
	// members, B2 channels, B7 calls). ResourceKind is the conversation's kind
	// (channel, dm, group: business/Conversation) and ResourceUUID its id; Data
	// names the message ("message", and "thread" for a reply's root), never its
	// text.
	MessageCreated        Kind = "message.created"
	MessageEdited         Kind = "message.edited"
	MessageDeleted        Kind = "message.deleted"
	MessageReacted        Kind = "message.reacted"
	ConversationCreated   Kind = "conversation.created"
	MemberAdded           Kind = "member.added"
	MemberRemoved         Kind = "member.removed"
	ChannelRenamed        Kind = "channel.renamed"
	ChannelTopicChanged   Kind = "channel.topic_changed" // Data: topic ("" removed it); B28
	ChannelPrivacyChanged Kind = "channel.privacy_changed"
	ChannelArchived       Kind = "channel.archived"
	ChannelRestored       Kind = "channel.restored"
	CallStarted           Kind = "call.started"
	CallEnded             Kind = "call.ended"
	// Pins (chat B25): Data names the message. Declared for the webhooks
	// catalogue; business/Pin tells open apps over MQTT until the pipeline
	// emits them.
	MessagePinned   Kind = "message.pinned"
	MessageUnpinned Kind = "message.unpinned"

	// The calendar (design/calendar-vs-gcal/PROGRAM.md §5), declared at its day
	// 0 and emitted by the packages that build them (A1 events, A2 answers, A5
	// reminders, A6 sharing, B5 and B9 bookings, A4 and B6 to B8 sync).
	// ResourceKind is "event", "calendar" or "booking" and ResourceUUID its id;
	// Data names what changed, never an event's notes.
	CalendarEventCreated   Kind = "event.created"
	CalendarEventUpdated   Kind = "event.updated"
	CalendarEventCancelled Kind = "event.cancelled"
	CalendarEventDeleted   Kind = "event.deleted"
	CalendarEventRestored  Kind = "event.restored"
	CalendarEventRSVP      Kind = "event.rsvp"
	CalendarEventReminder  Kind = "event.reminder"
	CalendarShared         Kind = "calendar.shared"
	CalendarUnshared       Kind = "calendar.unshared"
	BookingCreated         Kind = "booking.created"
	BookingCancelled       Kind = "booking.cancelled"
	BookingRescheduled     Kind = "booking.rescheduled"
	BookingRequested       Kind = "booking.requested"
	CalendarSyncFailed     Kind = "sync.failed"

	// Tables (design/tables-vs-airtable/PROGRAM.md §5), declared at its day 0
	// and emitted by the packages that build them (A2 rows, trash and fields,
	// A5 bases and sharing, B3 views, B11 comments). ResourceKind is "table"
	// (a base's, "base") and ResourceUUID its id; Data names the change set,
	// its kind, the rows and the fields it changed, never a cell's value.
	// Today's table.row.created and table.row.updated webhooks stay as they
	// are until B10 moves their subscribers.
	TableCreated      Kind = "table.created"
	TableDeleted      Kind = "table.deleted" // to the trash
	TableRestored     Kind = "table.restored"
	TableRowsChanged  Kind = "table.rows.changed"
	TableRowsDeleted  Kind = "table.rows.deleted" // to the table's trash
	TableRowsRestored Kind = "table.rows.restored"
	TableFieldChanged Kind = "table.field.changed"
	TableViewChanged  Kind = "table.view.changed"
	TableCommentAdded Kind = "table.comment.added"
	BaseCreated       Kind = "base.created"
	BaseShared        Kind = "base.shared"

	// Projects (design/projects-vs-asana-jira/PROGRAM.md §5), declared at its
	// day 0 and emitted by the packages that build them (A1 membership, A3
	// tasks through the Task Writer, B1 triage, B2 SLAs, B3 cycles, B10
	// releases). ResourceKind is "task", "project", "cycle" or "release" and
	// ResourceUUID its id; Data names what changed ("fields", "role",
	// "from_project"), never a description or a comment.
	TaskCreated          Kind = "task.created"
	TaskChanged          Kind = "task.changed"
	TaskMoved            Kind = "task.moved"
	TaskDeleted          Kind = "task.deleted"
	TaskRestored         Kind = "task.restored"
	ProjectChanged       Kind = "project.changed"
	ProjectMemberAdded   Kind = "project.member.added"
	ProjectMemberRemoved Kind = "project.member.removed"
	CycleStarted         Kind = "cycle.started"
	CycleEnded           Kind = "cycle.ended"
	TriageDecided        Kind = "triage.decided"
	SLAAtRisk            Kind = "sla.at_risk"
	SLABreached          Kind = "sla.breached"
	ReleaseStage         Kind = "release.stage"

	// Calls (design/calls-vs-zoom/PROGRAM.md §5), emitted by Call State
	// (business/CallState) beside chat's CallStarted and CallEnded above.
	// ResourceKind is the conversation's kind, or "meeting" for an instant
	// meeting, and ResourceUUID its id (a DM's or group's grouping id). Data
	// names the call (session, room, kind) and the person ("identity"), never
	// what was said. CallJoined fires at every join, with rejoin=true after the
	// first; CallLeft at every leave.
	CallJoined             Kind = "call.joined"
	CallLeft               Kind = "call.left"
	CallParticipantRemoved Kind = "call.participant.removed"
	CallLocked             Kind = "call.locked"
	CallUnlocked           Kind = "call.unlocked"
	CallRoleChanged        Kind = "call.role_changed"
)

// Actor is who made the change, from server context (I11): for an agent, the
// person who asked it, with the agent beside them.
type Actor struct {
	UserUUID  string `json:"user_uuid,omitempty"`
	AgentUUID string `json:"agent_uuid,omitempty"`
	// System is a change the server made on its own: a sweep, a retention rule.
	System bool `json:"system,omitempty"`
}

// Event is one change to one resource.
type Event struct {
	Kind         Kind      `json:"kind"`
	ResourceKind string    `json:"resource_kind"`
	ResourceUUID string    `json:"resource_uuid"`
	Actor        Actor     `json:"actor"`
	At           time.Time `json:"at"`
	// Subjects are the people the event is about, beyond its followers: whom it
	// was shared with, who was mentioned.
	Subjects []string `json:"subjects,omitempty"`
	// Data is the kind's own detail ("title", "from_parent", "level", "version").
	// Never a body: events travel to webhooks and the audit log.
	Data map[string]string `json:"data,omitempty"`
}

// Emitter takes events. Emit never fails the change that caused it and never
// waits for a sink. Each sink that wants an event gets it at most once: a
// delivery that fails, panics or runs out of time is logged, and is not tried
// again.
type Emitter interface {
	Emit(ctx context.Context, e Event)
}

// Bus is where every event goes: to every sink registered in Sinks.
var Bus Emitter = fanOut{sinks: Sinks}
