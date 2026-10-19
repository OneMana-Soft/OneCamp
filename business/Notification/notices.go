package notification

// Telling people about a Resource Event in all three ways, each on its own: in
// the inbox, by push, and by email (docs programme A6).
//
// Who to tell is decided before this (business/Follow's rules, business/Comment's
// participants): Tell is handed the people and why each is told. It writes each
// one's inbox notice under a key that names the telling, and only a notice it
// wrote goes further: published live to their open apps, pushed to their devices
// with a desktop notice beside it, as every push goes (Deliver: its gate holds
// back anyone deactivated, paused, in quiet hours or in focus time), and emailed
// by the rules every email follows (Dispatch). So an event delivered twice, or a
// save repeated, tells nobody twice. A push that fails leaves the email, and an
// email that fails leaves the push (B4).

import (
	"context"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"strings"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
	"github.com/google/uuid"
)

// The email kinds of the docs programme's notices.
const (
	EventDocMention  EventType = "doc.mention"
	EventDocShared   EventType = "doc.shared"
	EventDocProposal EventType = "doc.proposal"
	// A mention in a comment on a table's row, and a reply in a thread on
	// one (business/TableComments).
	EventTableMention EventType = "table.mention"
	EventTableComment EventType = "table.comment"
)

// ActivityNotice is the activity feed's type for a notice.
const ActivityNotice = "NOTICE"

// Notice is what to tell people, and how it reads.
type Notice struct {
	// Kind is the Resource Event's kind ("doc.mentioned").
	Kind         string
	ResourceKind string
	ResourceUUID string
	// ResourceTitle is shown to people who were told: they can open the thing.
	ResourceTitle string
	Actor         *dgraphStruct.DgraphUser
	Agent         *dgraphStruct.DgraphUser
	// Data is ids only, kept with the notice ("proposal_id", "comment_uuid").
	Data map[string]string
	// DedupKey names this telling; each person's notice adds their id to it.
	DedupKey string

	// Title is the sentence a push and an email lead with: "Akash mentioned
	// you in Launch notes". Body is a line under it, never a doc's body.
	Title string
	Body  string
	// Path is where it opens in the app: "/app/doc/<id>".
	Path string
	// Email is the kind of email it is ("" sends none); its toggle decides.
	Email EventType
	// Push is where its push opens, for a notice about something other than
	// a doc; nil opens the doc ResourceUUID names.
	Push *Push
	// Closed is a notice its recipients can't open the thing from (a request
	// for access that wasn't approved): its live item names nothing and opens
	// nothing, as the inbox's reading of it does (business/Follow feed.go).
	// Title, Body and ResourceTitle must say nothing about the thing then.
	Closed bool
}

// Push is where a notice's push opens, and how it shows.
type Push struct {
	// Type is the app's push type (firebaseInit.FIREBASE_PUSH_DATA_TYPE_*),
	// and ID what it opens: "" for the type's own page.
	Type string
	ID   string
	// Tag makes a later push with the same tag replace this one.
	Tag string
	// Thread is a second id the push opens within ID (its thread_id): a
	// table's row within the table.
	Thread string
	// From is who it is from when the notice has no actor: a guest outside
	// the workspace.
	From string
}

// NoticeRecipient is one person to tell, and why.
type NoticeRecipient struct {
	UserUUID string
	// Reason is why: "mention", "participant", "follower", "subject", "owner".
	Reason string
	// InboxOnly is told in the inbox alone: no push, no email.
	InboxOnly bool
	// Listed is pushed and emailed, but has no inbox item: their feed lists
	// the thing already (a comment on what they made, a mention of them). A
	// hidden row still keeps them from being told twice.
	Listed bool
	// NoPush is told in the inbox and by email, never by push, desktop
	// notice or sound: an organiser hearing of a guest's answer, who asked to
	// be emailed answers (calendar D-CAL-17).
	NoPush bool
	// ThroughHolds is pushed through a pause, quiet hours and focus time
	// (DeliverThroughPause): a reminder of an event they said yes to, which
	// starts inside them (calendar D-CAL-4). Never someone deactivated.
	ThroughHolds bool
}

// Seams for tests: where notices, pushes, live updates and emails go.
var (
	noticeWrite   = followModels.InsertNotices
	noticeActive  = userDomain.GetActiveUsersByUUIDsForNotification
	noticeDeliver = Deliver
	// noticeDeliverThrough is Deliver past a person's holds (ThroughHolds).
	noticeDeliverThrough = func(ctx context.Context, recipients []string, pushData map[string]string, noticeID string) {
		if err := DeliverThroughPause(ctx, recipients, pushData, noticeID); err != nil {
			helpers.LogErrorWithContext(ctx, "notification/Tell %s through holds: %+v", noticeID, err)
		}
	}
	noticeLive = func(userUUID string, item *dgraphModels.UnifiedActivityItem) {
		mqttBusiness.PublishMessageToUser(userUUID, mqttStruct.MESSAGE_NOTICE, mqttStruct.MqttNotice{UserUuid: userUUID, Activity: item})
	}
	noticeEmail = Dispatch
	noticesOn   = func() bool { return postgresInit.DBConn != nil && postgresInit.DBConn.SqlDB != nil }
)

// Tell tells each recipient once (see the file's comment) and answers who was
// newly told. The actor is never told of their own change.
func Tell(ctx context.Context, n Notice, recipients []NoticeRecipient) []string {
	if len(recipients) == 0 || !noticesOn() {
		return nil
	}
	actorUUID := ""
	if n.Actor != nil {
		actorUUID = n.Actor.Uuid
	}
	agentUUID := ""
	if n.Agent != nil {
		agentUUID = n.Agent.Uuid
	}
	type asked struct {
		id uuid.UUID
		r  NoticeRecipient
	}
	var who []asked
	var ids []uuid.UUID
	seen := map[string]bool{"": true, actorUUID: true}
	for _, r := range recipients {
		id, err := uuid.Parse(r.UserUUID)
		if err != nil || seen[id.String()] {
			continue
		}
		seen[id.String()] = true
		who, ids = append(who, asked{id, r}), append(ids, id)
	}
	if len(who) == 0 {
		return nil
	}
	// A deactivated person keeps their grants and follows, so the people a
	// change concerns can include them: they're told nothing, inbox and all,
	// as an email isn't sent them (Dispatch reads active people alone). Who
	// is active unread tells nobody, as for a push and an email.
	active, err := noticeActive(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "notification/Tell %s: who is active couldn't be read, telling nobody: %+v", n.Kind, err)
		return nil
	}
	type toTell struct {
		row *followModels.Notice
		r   NoticeRecipient
	}
	var rows []*followModels.Notice
	var tells []toTell
	for _, a := range who {
		if active[a.id] == nil {
			continue
		}
		reason := a.r.Reason
		if a.r.Listed {
			reason = followModels.ReasonListed
		}
		row := &followModels.Notice{
			ID:           uuid.New(),
			UserUUID:     a.id,
			Kind:         n.Kind,
			ResourceKind: n.ResourceKind,
			ResourceUUID: n.ResourceUUID,
			ActorUUID:    actorUUID,
			AgentUUID:    agentUUID,
			Reason:       reason,
			Data:         n.Data,
			DedupKey:     n.DedupKey + "|" + a.id.String(),
			CreatedAt:    time.Now().UTC(),
		}
		rows, tells = append(rows, row), append(tells, toTell{row, a.r})
	}
	// One statement for the lot; only a notice that was written goes further.
	written, err := noticeWrite(ctx, rows)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "notification/Tell %s: the notices couldn't be written, telling nobody: %+v", n.Kind, err)
		return nil
	}
	var told, reach, pushed, through []string
	for _, t := range tells {
		if !written[t.row.UserUUID] {
			continue
		}
		id := t.row.UserUUID.String()
		told = append(told, id)
		if !t.r.Listed {
			item := NoticeItem(t.row, n.Actor, n.Agent, n.ResourceTitle, !n.Closed)
			noticeLive(id, &item)
		}
		if !t.r.InboxOnly {
			reach = append(reach, id)
			switch {
			case t.r.NoPush:
			case t.r.ThroughHolds:
				through = append(through, id)
			default:
				pushed = append(pushed, id)
			}
		}
	}
	if len(reach) == 0 {
		return told
	}
	if len(pushed) > 0 {
		pushNotice(ctx, n, pushed, noticeDeliver)
	}
	if len(through) > 0 {
		pushNotice(ctx, n, through, noticeDeliverThrough)
	}
	emailNotice(ctx, n, reach)
	return told
}

// NoticeItem is a written notice as the inbox lists it, live (Tell) or read
// back (business/Follow's feed), one shape for both: the people shown as an
// inbox shows them (DgraphUser.Public, never an email), the actor's kind as the feed
// has always said it (activity.ActorKind: an agent, or an app for a bridge),
// and what it is about titled and openable only when the reader may open it
// (canOpen). The live item was built apart from the feed's and said "person"
// for any actor without an agent, so a bot's notice moved between the people
// and agents filters when the inbox was read again.
func NoticeItem(row *followModels.Notice, actor, agent *dgraphStruct.DgraphUser, title string, canOpen bool) dgraphModels.UnifiedActivityItem {
	at := row.CreatedAt.UTC().Format(time.RFC3339Nano)
	kind := activityBusiness.ActorPerson
	if actor != nil {
		c := *actor // ActorKind reads the email, then clears it
		kind = activityBusiness.ActorKind(&c)
	}
	if agent != nil {
		kind = activityBusiness.ActorAgent
	}
	item := &dgraphModels.NoticeItem{
		ID:           row.ID.String(),
		Kind:         row.Kind,
		ResourceKind: row.ResourceKind,
		ResourceUUID: row.ResourceUUID,
		Reason:       row.Reason,
		Actor:        actor.Public(),
		Agent:        agent.Public(),
		Data:         row.Data,
		CreatedAt:    at,
	}
	if canOpen {
		item.Title, item.CanOpen = title, true
	}
	return dgraphModels.UnifiedActivityItem{
		ActivityType: ActivityNotice,
		Time:         at,
		Priority:     "normal",
		ActorKind:    kind,
		Notice:       item,
	}
}

// openTitle is the title a live item shows: none on a closed notice.
func openTitle(n Notice) string {
	if n.Closed {
		return ""
	}
	return n.ResourceTitle
}

// pushNotice tells the recipients as every push does (Deliver): by person,
// through the push gate (never anyone deactivated, paused, in quiet hours or in
// focus time), a push to each of their devices and a desktop notice beside it,
// for the desktop app, which has no web push (TN-BUG-12). It had a loop of its
// own whose device lookup asked only about a pause, so a deactivated doc owner
// or follower was pushed the doc's title and a reply's words, and the desktop
// app heard of no notice at all. The desktop notice's id is the telling's key,
// as the email's dedup part is, so the app raises it once.
func pushNotice(ctx context.Context, n Notice, recipients []string, deliver func(ctx context.Context, recipients []string, pushData map[string]string, noticeID string)) {
	name := ""
	if n.Actor != nil {
		name = n.Actor.DisplayName()
	}
	kind, id := firebaseInit.FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT, n.ResourceUUID
	if p := n.Push; p != nil {
		kind, id = p.Type, p.ID
		if name == "" {
			name = p.From
		}
	}
	data := map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:     kind,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:  id,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:    n.Title,
		firebaseInit.FIREBASE_PUSH_DATA_BODY:     n.Body,
		firebaseInit.FIREBASE_PUSH_DATA_USERNAME: name,
	}
	if n.Push != nil && n.Push.Tag != "" {
		data[firebaseInit.FIREBASE_PUSH_DATA_TAG] = n.Push.Tag
	}
	if n.Push != nil && n.Push.Thread != "" {
		data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] = n.Push.Thread
	}
	deliver(ctx, recipients, data, n.DedupKey)
}

// emailNotice emails the recipients, by every rule an email follows.
func emailNotice(ctx context.Context, n Notice, recipients []string) {
	if n.Email == "" {
		return
	}
	name, id := "", ""
	if n.Actor != nil {
		name, id = n.Actor.DisplayName(), n.Actor.Uuid
	}
	noticeEmail(ctx, Event{
		Type:          n.Email,
		ActorUUID:     id,
		ActorName:     actorOrSomeone(name),
		SubjectLine:   n.Title,
		Title:         n.Title,
		Subtitle:      strings.TrimSpace(openTitle(n)),
		Body:          trimSnippet(n.Body, 600),
		CTAURL:        frontendBaseURL() + n.Path,
		CTAText:       "Open in OneCamp",
		DedupKeyParts: []string{n.DedupKey},
		Recipients:    recipientsFromStrings(recipients),
	})
}
