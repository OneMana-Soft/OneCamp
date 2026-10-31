package business

// Who an outgoing webhook may send anything about (audit A9, package 21).
//
// A webhook sends what happens in the workspace to an address its owner chose:
// its created_by, who set it up, pointed it somewhere new, or took it over.
// Only a doc's events (the Access Resolver) and a restricted task's (taskread)
// were held to what that person may read. An admin outside a private channel
// could set a webhook up for it and be sent its messages whole, a workspace
// webhook was sent every project's tasks whatever its owner's role there, and
// an admin could point another admin's webhook at their own server.
//
// reachOf is the one rule now. It holds a webhook to what its owner may read:
// the channel or project it is set up for, and everything an event names (its
// channel, its project, a doc's event's doc, and a task its project keeps to a
// few people). It is asked
//   - when a webhook is set up, set up for somewhere else, or pointed at a new
//     address (webhookScope): a channel or project its owner can't read is
//     refused;
//   - by every dispatch, for each webhook: an event its owner can't read is
//     sent to it not at all, not even as ids;
//   - again as each delivery is tried (business/Hooks, R2): an owner who has
//     left, or can no longer read the channel or project the webhook is set
//     up for, pauses it with why until an admin who can read it takes it over;
//   - for whoever reads a webhook's log: a body they couldn't read shows as
//     its ids (shownTo).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	hooksBusiness "github.com/akashc777/OneCamp/business/Hooks"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	deliveryModel "github.com/akashc777/OneCamp/models/postgres/WebhookDelivery"
	logModel "github.com/akashc777/OneCamp/models/postgres/WebhookLog"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// needs is what someone must be able to read to be sent an event, or to own a
// webhook set up for a place.
type needs struct {
	channel, project string
	// doc is a doc's event's doc (business/Follow), and docEvent says the
	// event is one: a doc's event that names no doc reaches nobody.
	doc      string
	docEvent bool
	// task is a task its project keeps to a few people (WA2).
	task string
	// meeting is a meeting's call's event's meeting (business/CallState
	// webhooks.go), and callEvent says the event is a call's: one in a channel
	// is held to the channel, one in a meeting to who may read the meeting, and
	// one that names neither reaches nobody.
	meeting   string
	callEvent bool
}

// needsOf is what an event names. Whether its task is kept to a few people is
// read, so ctx goes along.
func needsOf(ctx context.Context, eventType string, data map[string]interface{}) needs {
	n := needs{channel: named(data, "channel_id"), project: named(data, "project_id")}
	if strings.HasPrefix(eventType, "doc.") {
		n.docEvent, n.doc = true, named(data, "doc_uuid")
	}
	if strings.HasPrefix(eventType, "call.") {
		n.callEvent, n.meeting = true, named(data, "meeting_id")
	}
	if task := taskOfEvent(data); task != "" && restrictedTaskEvent(ctx, data) {
		n.task = task
	}
	return n
}

// scopeNeeds is what a webhook set up for scope (and entity) needs its owner to
// read: nothing for the whole workspace.
func scopeNeeds(scope string, entity *uuid.UUID) needs {
	switch {
	case entity == nil:
		return needs{}
	case scope == ScopeChannel:
		return needs{channel: entity.String()}
	case scope == ScopeProject:
		return needs{project: entity.String()}
	}
	return needs{}
}

func named(data map[string]interface{}, key string) string {
	s, _ := data[key].(string)
	return strings.TrimSpace(s)
}

// errNoChannelRule is a channel asked about before business/Channel said who
// may read one.
var errNoChannelRule = errors.New("webhooks: nothing says who may read a channel")

// errNoMeetingRule is a meeting asked about before business/CallState said who
// may read one.
var errNoMeetingRule = errors.New("webhooks: nothing says who may read a meeting")

// The rules reachOf is built from, as seams for tests.
var (
	memberNow = hooksBusiness.MemberNow
	// readsChannel is business/Channel's ReadableBy, which it sets
	// (ReadChannelsWith): it imports this package, so this one can't call it.
	// Until it does, nobody reads a channel.
	readsChannel = func(context.Context, string, string) (bool, error) { return false, errNoChannelRule }
	// readsMeeting is business/CallState's MayRead for a meeting, which it
	// sets (ReadMeetingsWith), as business/Channel sets readsChannel.
	readsMeeting = func(context.Context, string, string) (bool, error) { return false, errNoMeetingRule }
	graphUser    = userDomain.GetActiveDgraphUserInfoByUUID
	projectAs    = projectDomain.GetBasicDgraphProjectInfo
	standingIn   = workAccess.Of
	resolveDoc   = func(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
		return accessBusiness.Rules.Resolve(ctx, who, res)
	}
	hiddenTasks = taskread.For
)

// ReadChannelsWith sets who may read a channel's messages: business/Channel's
// rule.
func ReadChannelsWith(f func(ctx context.Context, userUUID, channelID string) (bool, error)) {
	readsChannel = f
}

// ReadMeetingsWith sets who may read a meeting's calls: business/CallState's
// rule.
func ReadMeetingsWith(f func(ctx context.Context, userUUID, meetingID string) (bool, error)) {
	readsMeeting = f
}

// reachOf is how an event that names event stands for a webhook set up for
// scope, with who as its owner (reader.reach), asked once.
func reachOf(ctx context.Context, who uuid.UUID, scope, event needs) (hooksBusiness.Reach, error) {
	return newReader(who).reach(ctx, scope, event)
}

// reader asks what one person may read, each thing once: a dispatch keeps one
// per webhook owner, and a log page one for its viewer. A lookup that failed
// isn't kept.
type reader struct {
	who    uuid.UUID
	member *bool
	uid    string
	seen   map[string]bool
	hidden *taskread.Filter
}

func newReader(who uuid.UUID) *reader { return &reader{who: who, seen: map[string]bool{}} }

// reach is how an event that names event stands for a webhook set up for scope
// with them as its owner: gone, unable to read what the webhook is set up for,
// unable to read what the event names, or reached. An error is a lookup that
// failed, which decides nothing.
func (r *reader) reach(ctx context.Context, scope, event needs) (hooksBusiness.Reach, error) {
	if r.member == nil {
		member, err := memberNow(ctx, r.who)
		if err != nil {
			return 0, err
		}
		r.member = &member
	}
	if !*r.member {
		return hooksBusiness.OwnerGone, nil
	}
	ok, err := r.reads(ctx, scope)
	if err != nil {
		return 0, err
	}
	if !ok {
		return hooksBusiness.OwnerOutOfScope, nil
	}
	if ok, err = r.reads(ctx, event); err != nil {
		return 0, err
	}
	if !ok {
		return hooksBusiness.OwnerOutOfEvent, nil
	}
	return hooksBusiness.Reaches, nil
}

func (r *reader) graphUID(ctx context.Context) (string, error) {
	if r.uid == "" {
		u, err := graphUser(ctx, r.who.String())
		if err != nil || u == nil {
			return "", err
		}
		r.uid = u.Uid
	}
	return r.uid, nil
}

// once answers what ask says of key, asking the first time.
func (r *reader) once(key string, ask func() (bool, error)) (bool, error) {
	if ok, known := r.seen[key]; known {
		return ok, nil
	}
	ok, err := ask()
	if err != nil {
		return false, err
	}
	r.seen[key] = ok
	return ok, nil
}

// reads is whether they may read everything n names: its channel (who may read
// its messages), its project (a role of viewer or above, as Work Access
// decides), its doc (the Access Resolver; a doc in the trash, as they could
// before), its meeting (who may read its calls), and its task, when its
// project keeps it to a few people.
func (r *reader) reads(ctx context.Context, n needs) (bool, error) {
	if n.channel != "" {
		ok, err := r.once("channel "+n.channel, func() (bool, error) { return readsChannel(ctx, r.who.String(), n.channel) })
		if err != nil || !ok {
			return false, err
		}
	}
	if n.callEvent && n.channel == "" {
		if n.meeting == "" {
			return false, nil
		}
		ok, err := r.once("meeting "+n.meeting, func() (bool, error) { return readsMeeting(ctx, r.who.String(), n.meeting) })
		if err != nil || !ok {
			return false, err
		}
	}
	if n.project != "" {
		ok, err := r.once("project "+n.project, func() (bool, error) {
			uid, err := r.graphUID(ctx)
			if err != nil || uid == "" {
				return false, err
			}
			p, err := projectAs(ctx, n.project, uid)
			if err != nil {
				return false, err
			}
			s, err := standingIn(ctx, r.who.String(), p)
			return err == nil && s.Can(workAccess.NeedView), err
		})
		if err != nil || !ok {
			return false, err
		}
	}
	if n.docEvent {
		if n.doc == "" {
			return false, nil
		}
		ok, err := r.once("doc "+n.doc, func() (bool, error) {
			uid, err := r.graphUID(ctx)
			if err != nil || uid == "" {
				return false, err
			}
			d, err := resolveDoc(ctx, accessBusiness.Subject{UserUID: uid, UserUUID: r.who.String()}, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: n.doc})
			return err == nil && (d.Level >= accessBusiness.LevelView || d.Deleted && d.Was >= accessBusiness.LevelView), err
		})
		if err != nil || !ok {
			return false, err
		}
	}
	if n.task != "" {
		if r.hidden == nil {
			f, err := hiddenTasks(ctx, r.who.String())
			if err != nil {
				return false, err
			}
			r.hidden = &f
		}
		if r.hidden.Hides(n.task) {
			return false, nil
		}
	}
	return true, nil
}

// sent is a delivery's body read back (OutgoingWebhookPayload). Data that isn't
// an object names nothing, as dispatch reads it.
type sent struct {
	Event     string          `json:"event"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
	EventID   string          `json:"event_id,omitempty"`
}

func readSent(body []byte) (sent, map[string]interface{}, error) {
	var p sent
	if err := json.Unmarshal(body, &p); err != nil {
		return p, nil, err
	}
	var data map[string]interface{}
	_ = json.Unmarshal(p.Data, &data)
	return p, data, nil
}

// deliveryReach is how a delivery's body stands for who, for business/Hooks as
// it tries it (R2); a nil body is the webhook as it is set up.
func deliveryReach(ctx context.Context, who uuid.UUID, t deliveryModel.Target, payload []byte) (hooksBusiness.Reach, error) {
	var event needs
	if payload != nil {
		p, data, err := readSent(payload)
		if err != nil {
			return 0, err
		}
		event = needsOf(ctx, p.Event, data)
	}
	return reachOf(ctx, who, scopeNeeds(t.ScopeType, t.ScopeEntity), event)
}

func init() { hooksBusiness.AnswerReachWith(deliveryReach) }

// shownTo cuts each log's body, and the endpoint's answer to it, to what
// viewer may read: a body about something they couldn't read shows as its
// ids, with no answer. The log holds what was sent, and an admin reading
// another admin's webhook's log was shown a private channel's messages.
func shownTo(ctx context.Context, viewer uuid.UUID, logs []*logModel.WebhookLog) {
	r := newReader(viewer)
	for _, l := range logs {
		if l.RequestBody == nil {
			continue
		}
		p, data, err := readSent([]byte(*l.RequestBody))
		if err != nil {
			l.RequestBody, l.ResponseBody = nil, nil
			continue
		}
		if reach, err := r.reach(ctx, needs{}, needsOf(ctx, p.Event, data)); err == nil && reach == hooksBusiness.Reaches {
			continue
		}
		cut, err := json.Marshal(OutgoingWebhookPayload{Event: p.Event, Timestamp: p.Timestamp, Data: idsOnly(data), EventID: p.EventID})
		if err != nil {
			l.RequestBody = nil
		} else {
			s := string(cut)
			l.RequestBody = &s
		}
		l.ResponseBody = nil
	}
}
