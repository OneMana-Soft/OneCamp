package business

// A DM's or a small group's call rings its people (slack-06, head H2 "Ring
// in DMs"): when the call starts (call.started, from business/CallState),
// each person it rings is told on their own activity topic, so every device
// they have open rings at once, until ring_until, until they join or say no
// (POST /calls/{session_id}/decline), or until the call ends. The push, the
// desktop notice and the email of callStartedPush.go stay as they are: this
// rings an open app, they reach a closed one.
//
// Who is rung (whoRings): the conversation's people but the caller, never a
// bot (V-126), never an external guest, never someone deactivated; a group
// of more than RingGroupMax people (its guests count, its bots don't) rings
// nobody (the push alone). Someone the conversation's push wouldn't reach now
// (muted there, or paused) is rung quietly: their devices show the call
// without a tone.
//
// Nothing is stored: the end stops whoever whoRings answers then, the
// starter too (a ring again may have rung them once they left), so someone
// who joined the conversation during the call hears a stop that matches no
// ring, which their app passes over; someone who left it stops at ring_until.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	notifyBusiness "github.com/akashc777/OneCamp/business/Notify"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

const (
	// RingFor is how long a call rings: ring_until is its start and this.
	RingFor = 30 * time.Second
	// RingGroupMax is the most people a group may have for its calls to
	// ring; a bigger group's call is told by the push alone.
	RingGroupMax = 8
	// RingAgainMax is how many times someone in a call may ring its people
	// again (POST /calls/{session_id}/ring).
	RingAgainMax = 3
)

var (
	// ErrStillRinging is a ring again while the call's last ring still rings.
	ErrStillRinging = errors.New("chat: the call is still ringing")
	// ErrRingLimit is a ring again past RingAgainMax.
	ErrRingLimit = errors.New("chat: the call was rung again as often as it may be")
)

// ringState is what a call's rings have been: until when the last one rings,
// and how many times it was rung again.
type ringState struct {
	Until time.Time
	Again int
}

// RingingCall is a DM's or group's call going on, as Call State reads it, for
// a ring again: its session, conversation, start, and the people in it now.
type RingingCall struct {
	Session, Kind, Resource string
	StartedAt               time.Time
	InCall                  []string
}

// rings is each ringing call's ringState, and whom its rings again rang
// since they last declined, in memory, as the sinks hold theirs. A restart
// forgets it: a ring again then reads the call's first ring from its start
// and counts its rings again from none, so at most RingAgainMax more.
var rings = struct {
	sync.Mutex
	m map[string]*ringMemo
}{m: map[string]*ringMemo{}}

type ringMemo struct {
	ringState
	rungAgain map[string]bool
}

// forgetRingsAfter is how long past its last ring a call's memo is kept
// when its end was never heard.
const forgetRingsAfter = time.Hour

// remember notes that the call session rings until then, and, for a ring
// again (again), whom; it forgets every memo long past its last ring. The
// caller holds rings' lock.
func remember(session string, st ringState, again []rung, now time.Time) {
	memo := rings.m[session]
	if memo == nil {
		memo = &ringMemo{rungAgain: map[string]bool{}}
		rings.m[session] = memo
	}
	memo.ringState = st
	for _, r := range again {
		memo.rungAgain[strings.ToLower(r.UUID)] = true
	}
	for id, m := range rings.m {
		if now.Sub(m.Until) > forgetRingsAfter {
			delete(rings.m, id)
		}
	}
}

// forgetRings forgets a call's rings, at its end.
func forgetRings(session string) {
	rings.Lock()
	delete(rings.m, session)
	rings.Unlock()
}

// DeclinesRingAgain is whether user's decline of the call session answers a
// ring again that rang them (their first decline is told anyway), and then
// it no longer does: a repeat tells nobody again.
func DeclinesRingAgain(session, user string) bool {
	rings.Lock()
	defer rings.Unlock()
	memo := rings.m[session]
	key := strings.ToLower(user)
	if memo == nil || !memo.rungAgain[key] {
		return false
	}
	delete(memo.rungAgain, key)
	return true
}

// RingAgain rings again, at now, the people of the call c who aren't in it,
// for asker, someone in it (Call State's MayRingAgain): whoever whoRings
// answers, a decliner too, since ringing again is the asker's own ask. Each
// rings until RingFor from now, and the asker is told who ("ringing"). It
// answers how many it rang: none, uncounted, when everyone is in already;
// ErrStillRinging while the last ring rings, and ErrRingLimit after
// RingAgainMax.
func RingAgain(ctx context.Context, c RingingCall, asker string, now time.Time) (int, error) {
	people, err := conversationDomain.PeopleOf(ctx, c.Resource)
	if err != nil {
		return 0, err
	}
	from := personOf(people, asker)
	if from == nil {
		return 0, nil
	}
	in := make(map[string]bool, len(c.InCall))
	for _, id := range c.InCall {
		in[strings.ToLower(id)] = true
	}
	var out []rung
	for _, r := range whoRings(people, asker, loudNow(ctx, c.Resource, asker)) {
		if !in[strings.ToLower(r.UUID)] {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return 0, nil
	}
	until := now.Add(RingFor)
	rings.Lock()
	st := ringState{Until: c.StartedAt.Add(RingFor)}
	if memo := rings.m[c.Session]; memo != nil {
		st = memo.ringState
	}
	if err := mayRingAgain(st, now); err != nil {
		rings.Unlock()
		return 0, err
	}
	remember(c.Session, ringState{Until: until, Again: st.Again + 1}, out, now)
	rings.Unlock()
	ringPeople(from, c.Session, mqttStruct.CallRingResource{Kind: c.Kind, UUID: c.Resource}, out, until, asker)
	return len(out), nil
}

// mayRingAgain is whether a call whose rings have been st may ring again at
// now: never past RingAgainMax (the answer that holds), nor while its last
// ring rings. Pure.
func mayRingAgain(st ringState, now time.Time) error {
	if st.Again >= RingAgainMax {
		return ErrRingLimit
	}
	if now.Before(st.Until) {
		return ErrStillRinging
	}
	return nil
}

func init() {
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "chat.call.ring", Wants: chatCallStarted, Run: ringChatPeople})
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "chat.call.ring.stop", Wants: chatCallRingStops, Run: stopChatRing})
}

// rung is one person a call rings, and whether without a tone.
type rung struct {
	UUID    string
	Name    string
	Profile string
	Quiet   bool
}

// whoRings is who a call in a conversation of these people rings when caller
// starts it, in the conversation's order: everyone but the caller, bots,
// external guests and anyone deactivated, and nobody at all in a
// conversation of more than RingGroupMax people (guests count, bots don't).
// Those not in loud (who the conversation's push reaches now) are rung
// quietly. Pure.
func whoRings(people []*dgraphStruct.DgraphUser, caller string, loud []string) []rung {
	live := make([]*dgraphStruct.DgraphUser, 0, len(people))
	n := 0 // the conversation's people: a guest is one, a bot isn't
	for _, p := range people {
		if p != nil && p.Uuid != "" && !helpers.IsSoftDeleted(p.DeletedAt) {
			live = append(live, p)
			if !p.IsBot {
				n++
			}
		}
	}
	if n > RingGroupMax {
		return nil
	}
	heard := make(map[string]bool, len(loud))
	for _, id := range loud {
		heard[strings.ToLower(id)] = true
	}
	var out []rung
	for _, p := range live {
		if strings.EqualFold(p.Uuid, caller) || p.IsBot || p.IsExternal {
			continue
		}
		profile := ""
		if p.ProfileKey != nil {
			profile = *p.ProfileKey
		}
		out = append(out, rung{UUID: p.Uuid, Name: p.DisplayName(), Profile: profile, Quiet: !heard[strings.ToLower(p.Uuid)]})
	}
	return out
}

// chatCallRingStops is what stops a DM's or a group's call ringing: someone
// joining it, or its end. Pure.
func chatCallRingStops(e resourceEventBusiness.Event) bool {
	return (e.Kind == resourceEventBusiness.CallJoined || e.Kind == resourceEventBusiness.CallEnded) &&
		(e.ResourceKind == "dm" || e.ResourceKind == "group")
}

// ringChatPeople rings the people of a DM's or group's call that just
// started, each on their own activity topic, as its caller is named in the
// conversation.
func ringChatPeople(ctx context.Context, e resourceEventBusiness.Event) {
	caller, session := e.Actor.UserUUID, e.Data["session"]
	if caller == "" || session == "" {
		return
	}
	people, err := conversationDomain.PeopleOf(ctx, e.ResourceUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Chat could not read who is in %s to ring them: %+v", e.ResourceUUID, err)
		return
	}
	from := personOf(people, caller)
	if from == nil {
		// Someone outside the conversation (in by a call's invite) rings nobody.
		return
	}
	whom := whoRings(people, caller, loudNow(ctx, e.ResourceUUID, caller))
	if len(whom) == 0 {
		return
	}
	started := e.At
	if t, err := time.Parse(time.RFC3339Nano, e.Data["started_at"]); err == nil {
		started = t
	}
	until := started.Add(RingFor)
	rings.Lock()
	remember(session, ringState{Until: until}, nil, time.Now())
	rings.Unlock()
	ringPeople(from, session, mqttStruct.CallRingResource{Kind: e.ResourceKind, UUID: e.ResourceUUID}, whom, until, caller)
}

// personOf is the one of people with the id who, or nil. Pure.
func personOf(people []*dgraphStruct.DgraphUser, who string) *dgraphStruct.DgraphUser {
	for _, p := range people {
		if p != nil && strings.EqualFold(p.Uuid, who) {
			return p
		}
	}
	return nil
}

// ringPeople rings each of whom on their own activity topic until then, as
// from calls them in the conversation where, and tells teller (who rings)
// whom it rings, so their call can show each one's answer.
func ringPeople(from *dgraphStruct.DgraphUser, session string, where mqttStruct.CallRingResource, whom []rung, until time.Time, teller string) {
	caller := mqttStruct.CallRingPerson{UUID: from.Uuid, Name: from.DisplayName()}
	if from.ProfileKey != nil {
		caller.Profile = *from.ProfileKey
	}
	ring := mqttStruct.MqttCallRing{State: mqttStruct.CallRingRing, Session: session, RingUntil: &until, Caller: &caller, Resource: &where}
	told := make([]mqttStruct.CallRingPerson, 0, len(whom))
	var loud, quiet []string
	for _, r := range whom {
		if r.Quiet {
			quiet = append(quiet, helpers.GetMqttTopicForUserActivity(r.UUID))
		} else {
			loud = append(loud, helpers.GetMqttTopicForUserActivity(r.UUID))
		}
		told = append(told, mqttStruct.CallRingPerson{UUID: r.UUID, Name: r.Name, Profile: r.Profile, Quiet: r.Quiet})
	}
	// One publish per kind of ring, not one per person: the loud ring and the
	// quiet one differ only in Quiet.
	if len(loud) > 0 {
		ring.Quiet = false
		mqttBusiness.PublishToTopics(loud, mqttStruct.MESSAGE_CALL_RING, ring)
	}
	if len(quiet) > 0 {
		ring.Quiet = true
		mqttBusiness.PublishToTopics(quiet, mqttStruct.MESSAGE_CALL_RING, ring)
	}
	mqttBusiness.PublishMessageToUser(teller, mqttStruct.MESSAGE_CALL_RING, mqttStruct.MqttCallRing{State: mqttStruct.CallRingRinging, Session: session, RingUntil: &until, People: told})
}

// loudNow is who of the conversation grp's people its call's push reaches
// now: the router's plan of the call (business/Notify, cause chat.call: those
// who hear everything there), and of them those it doesn't hold (pause, quiet
// hours, focus time), so the ring is loud for whom the call's push is sent
// now. A VIP's call passes a hold, as its push does. Nobody when the plan
// can't be made: everyone is rung, quietly.
func loudNow(ctx context.Context, grp, caller string) []string {
	ds, err := notifyBusiness.Notices.Plan(ctx, notifyBusiness.Event{Cause: notifyBusiness.CauseCall, Actor: caller,
		Place: notifyBusiness.Place{Kind: notifyBusiness.PlaceDM, ID: grp}})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Chat could not plan who in %s hears its calls now, so it rings quietly: %+v", grp, err)
		return nil
	}
	var loud []string
	for _, d := range ds {
		if slices.Contains(d.To, notifyBusiness.ToPush) {
			loud = append(loud, d.UserID)
		}
	}
	return loud
}

// stopChatRing stops a DM's or group's call ringing: whoever joined it, on
// every device, but for its starter's first join, which started it (nothing
// had rung them; a ring again may ring them once they left, so their join
// back stops it), or, at its end, everyone it may have rung, its starter too.
func stopChatRing(ctx context.Context, e resourceEventBusiness.Event) {
	session := e.Data["session"]
	id, err := uuid.Parse(session)
	if err != nil {
		return
	}
	s, err := callStateModel.SessionByID(ctx, id)
	if err != nil || s == nil {
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Chat could not read call %s to stop its ring: %+v", session, err)
		}
		return
	}
	starter := ""
	if s.StartedBy != nil {
		starter = s.StartedBy.String()
	}
	if e.Kind == resourceEventBusiness.CallJoined {
		if who := e.Actor.UserUUID; who != "" && (!strings.EqualFold(who, starter) || e.Data["rejoin"] == "true") {
			stopRinging(who, session)
		}
		return
	}
	forgetRings(session)
	people, err := conversationDomain.PeopleOf(ctx, e.ResourceUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Chat could not read who is in %s to stop its call's ring: %+v", e.ResourceUUID, err)
		return
	}
	var topics []string
	for _, r := range whoRings(people, "", nil) {
		topics = append(topics, helpers.GetMqttTopicForUserActivity(r.UUID))
	}
	if len(topics) > 0 {
		mqttBusiness.PublishToTopics(topics, mqttStruct.MESSAGE_CALL_RING, mqttStruct.MqttCallRing{State: mqttStruct.CallRingStop, Session: session})
	}
}

// stopRinging tells every device of someone's that the call session no longer
// rings them.
func stopRinging(user, session string) {
	mqttBusiness.PublishMessageToUser(user, mqttStruct.MESSAGE_CALL_RING, mqttStruct.MqttCallRing{State: mqttStruct.CallRingStop, Session: session})
}

// CallDeclined tells of a member's decline of a call ringing them (business/
// CallState Decline): their devices stop ringing, and the call's starter hears
// who said no. Nobody else is told.
func CallDeclined(session, starter string, by mqttStruct.CallRingPerson) {
	stopRinging(by.UUID, session)
	if starter == "" || strings.EqualFold(starter, by.UUID) {
		return
	}
	mqttBusiness.PublishMessageToUser(starter, mqttStruct.MESSAGE_CALL_RING, mqttStruct.MqttCallRing{State: mqttStruct.CallRingDeclined, Session: session, By: &by})
}
