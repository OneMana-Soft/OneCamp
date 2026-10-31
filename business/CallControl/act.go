package business

// Doing what someone running a call asks: mute, mute everyone, ask to
// unmute, remove, lock, end for everyone, co-hosts, spotlight, people joining
// muted, and what everyone else may do.

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	callLobbyBusiness "github.com/akashc777/OneCamp/business/CallLobby"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/livekit/protocol/livekit"
)

// The room metadata keys a call's settings live under, which every screen in
// the call (guests' too) reads.
const (
	metaLocked      = "locked"
	metaSpotlight   = "spotlight"
	metaMuteOnEntry = "muteOnEntry"
	// metaHeld is what the call holds for everyone who doesn't run it.
	metaHeld = "held"
)

// sourcesOf is the tracks a mute of a source turns off. Pure.
func sourcesOf(source string) []livekit.TrackSource {
	switch source {
	case "camera":
		return []livekit.TrackSource{livekit.TrackSource_CAMERA}
	case "screen_share":
		return []livekit.TrackSource{livekit.TrackSource_SCREEN_SHARE, livekit.TrackSource_SCREEN_SHARE_AUDIO}
	}
	return []livekit.TrackSource{livekit.TrackSource_MICROPHONE}
}

// sourceName is a source as the packets and the audit log say it. Pure.
func sourceName(source string) string {
	switch source {
	case "camera", "screen_share":
		return source
	}
	return "microphone"
}

// Act does what someone running a call asked, as the call's own rules allow
// (AbilitiesOf). A refusal is a *Refusal. The caller records what was done in
// the audit log.
func Act(ctx context.Context, actor *dgraphStruct.DgraphUser, sessionID string, a Action) (Result, error) {
	uid := ""
	if actor != nil {
		uid = actor.Uid
	}
	own := OwnAction(a, uid)
	need := abilityFor(a.Action)
	if !own && need == "" && !boardAction(a.Action) {
		return Result{}, ErrUnknownAction
	}
	if helpers.ActsThroughToken(ctx) {
		return Result{}, ErrThroughToken
	}
	if uid == "" {
		return Result{}, ErrNotInCall
	}
	if own {
		return ActOnYourself(ctx, uid, sessionID, a)
	}
	if need == CanBreakouts {
		return actOnBreakouts(ctx, actor, sessionID, a)
	}
	s, me, err := present(ctx, sessionID, actor.Uid)
	if err != nil {
		return Result{}, err
	}
	if s.Parent != "" && !roomAct(a.Action) {
		return Result{}, ErrMainRoomOnly
	}
	if boardAction(a.Action) {
		return actOnBoard(ctx, actor, s, me, a)
	}
	if !slices.Contains(AbilitiesOf(s.Resource.Kind, me.Role), need) {
		return Result{}, ErrNotAllowed
	}
	var target *callStateBusiness.Person
	if targeted(a.Action) {
		if target, err = targetOf(s, me, actor.Uid, a); err != nil {
			return Result{}, err
		}
	}
	by := resourceEventBusiness.Actor{UserUUID: actor.Uuid}
	switch a.Action {
	case ActMute:
		n, err := mute(ctx, s.Room, target.Identity, sourcesOf(a.Source))
		if err != nil {
			return Result{}, err
		}
		if n > 0 {
			tell(ctx, s.Room, liveKitBusiness.CallPacket{T: "muted", By: me.Name, Source: sourceName(a.Source)}, target.Identity)
			return Result{Affected: 1}, nil
		}
		return Result{}, nil
	case ActMuteAll:
		return muteAll(ctx, s, me)
	case ActAskUnmute:
		// Asked to unmute while microphones are held, they're let through first:
		// otherwise they couldn't.
		if !mayDo(s, target, model.HoldMicrophone) {
			if err := allow(ctx, s, target, model.HoldMicrophone, true); err != nil {
				return Result{}, err
			}
		}
		tell(ctx, s.Room, liveKitBusiness.CallPacket{T: "ask_unmute", By: me.Name}, target.Identity)
		return Result{Affected: 1}, nil
	case ActAskAllUnmute:
		if slices.Contains(s.Held, model.HoldMicrophone) {
			return Result{}, ErrMicsHeld
		}
		return askAllToUnmute(ctx, s, me)
	case ActRemove:
		err := liveKitBusiness.RemoveFromCall(ctx, s.Room, target.Identity, liveKitBusiness.Remover{Identity: actor.Uid, Name: me.Name}, liveKitBusiness.RemovedByHost)
		if errors.Is(err, liveKitBusiness.ErrNotInCall) {
			return Result{}, ErrTargetGone
		}
		if err != nil {
			return Result{}, err
		}
		callStateBusiness.NoteRemoved(ctx, s, target.Identity, by)
		return Result{Affected: 1}, nil
	case ActLock, ActUnlock:
		locked := a.Action == ActLock
		if err := settle(callStateBusiness.SetLock(ctx, s, locked, actor.Uid, by)); err != nil {
			return Result{}, err
		}
		var meta any
		if locked {
			meta = map[string]any{"by": me.Name, "since": time.Now().UnixMilli()}
		}
		return Result{Affected: 1}, setMeta(ctx, s.Room, metaLocked, meta)
	case ActEnd:
		// Told first, so every screen says who ended it rather than "disconnected".
		tell(ctx, s.Room, liveKitBusiness.CallPacket{T: "ended", By: me.Name})
		if _, err := callStateBusiness.EndForEveryone(ctx, s, by); settle(err) != nil {
			return Result{}, err
		}
		liveKitBusiness.DeleteRoom(ctx, s.Room)
		return Result{Affected: len(s.InCall())}, nil
	case ActMakeCohost, ActRemoveCohost:
		role := model.RoleCohost
		if a.Action == ActRemoveCohost {
			role = model.RoleParticipant
		}
		changed, err := callStateBusiness.SetRole(ctx, s, target.Identity, role, by)
		if err = settle(err); err != nil {
			return Result{}, err
		}
		if !changed {
			return Result{}, ErrTargetGone
		}
		tellPart(s.Room, s.Resource.Kind, target.Identity, role)
		tellSources(s.Room, target.Identity, liveKitBusiness.PublishSources(s.Held, target.Allowed, model.RunsCall(role)))
		tell(ctx, s.Room, liveKitBusiness.CallPacket{T: "role", Role: role, By: me.Name}, target.Identity)
		return Result{Affected: 1}, nil
	case ActSpotlight, ActEndSpotlight:
		identity := ""
		if a.Action == ActSpotlight {
			identity = target.Identity
		}
		if err := settle(callStateBusiness.SetSpotlight(ctx, s, identity)); err != nil {
			return Result{}, err
		}
		var meta any
		if identity != "" {
			meta = map[string]any{"identity": identity, "by": me.Name}
		}
		return Result{Affected: 1}, setMeta(ctx, s.Room, metaSpotlight, meta)
	case ActLowerHand:
		changed, _, err := callStateBusiness.SetHand(ctx, s, target.Identity, false)
		if err = settle(err); err != nil {
			return Result{}, err
		}
		if !changed {
			return Result{}, ErrHandDown
		}
		tellAttrs(s.Room, target.Identity, map[string]string{AttrHand: ""})
		tell(ctx, s.Room, liveKitBusiness.CallPacket{T: "hand_lowered", By: me.Name}, target.Identity)
		return Result{Affected: 1}, nil
	case ActLowerAll:
		lowered, err := callStateBusiness.LowerAllHands(ctx, s)
		if err = settle(err); err != nil {
			return Result{}, err
		}
		for _, identity := range lowered {
			tellAttrs(s.Room, identity, map[string]string{AttrHand: ""})
		}
		if len(lowered) > 0 {
			tell(ctx, s.Room, liveKitBusiness.CallPacket{T: "hand_lowered", By: me.Name, Reason: "all"}, lowered...)
		}
		return Result{Affected: len(lowered)}, nil
	case ActAdmit, ActDeny:
		changed, err := callLobbyBusiness.Decide(ctx, s.Room, a.Ticket, a.Action == ActAdmit, actor.Uid)
		if errors.Is(err, callLobbyBusiness.ErrNoKnock) {
			return Result{}, ErrNotWaiting
		}
		if err != nil {
			return Result{}, err
		}
		if !changed {
			return Result{}, ErrNotWaiting
		}
		return Result{Affected: 1}, nil
	case ActAdmitAll:
		n, err := callLobbyBusiness.AdmitAll(ctx, s.Room, actor.Uid)
		return Result{Affected: n}, err
	case ActMuteOnEntry:
		if err := settle(callStateBusiness.SetMuteOnEntry(ctx, s, a.On)); err != nil {
			return Result{}, err
		}
		var meta any
		if a.On {
			meta = true
		}
		return Result{Affected: 1}, setMeta(ctx, s.Room, metaMuteOnEntry, meta)
	case ActHold:
		return hold(ctx, s, me, a.What, a.On)
	case ActAllow:
		if !slices.Contains(model.Holds, a.What) {
			return Result{}, ErrNoSuchHold
		}
		if err := allow(ctx, s, target, a.What, a.On); err != nil {
			return Result{}, err
		}
		t := "allowed"
		if !a.On {
			t = "disallowed"
		}
		tell(ctx, s.Room, liveKitBusiness.CallPacket{T: t, By: me.Name, Source: a.What}, target.Identity)
		return Result{Affected: 1}, nil
	}
	return Result{}, ErrUnknownAction
}

// hold holds what (one of model.Holds) for everyone in the call who doesn't
// run it, or with on false lets them again: the call's record first, then
// what each of them may publish, then what every screen reads. Its open
// breakout rooms follow, each the same way (CR-62's SF2 on package C: a room
// took its call's holds as it opened, and then nothing). Answers how many
// people's publishing it changed. Done again, it reaches whoever an earlier
// try missed.
func hold(ctx context.Context, s *callStateBusiness.Session, me *callStateBusiness.Person, what string, on bool) (Result, error) {
	if !slices.Contains(model.Holds, what) {
		return Result{}, ErrNoSuchHold
	}
	// Only a channel's call has a chat (its post's thread, CALLS-3b).
	if what == model.HoldChat && s.Resource.Kind != conversationBusiness.KindChannel {
		return Result{}, ErrNoChat
	}
	n, err := holdIn(ctx, s, me, what, on)
	if err != nil {
		return Result{Affected: n}, err
	}
	rooms, err := callStateBusiness.OpenRoomsOf(ctx, s)
	if err != nil {
		return Result{Affected: n}, err
	}
	for _, r := range rooms {
		m, err := holdIn(ctx, r, me, what, on)
		n += m
		if err != nil {
			return Result{Affected: n}, err
		}
	}
	return Result{Affected: n}, nil
}

// holdIn holds what in one room of a call, its own or one of its breakout
// rooms: the room's record, what each person in it may publish, what its
// screens read and its board. Answers how many people's publishing it changed.
func holdIn(ctx context.Context, s *callStateBusiness.Session, me *callStateBusiness.Person, what string, on bool) (int, error) {
	held, err := callStateBusiness.SetHeld(ctx, s, what, on)
	if err = settle(err); err != nil {
		return 0, err
	}
	people, err := livekitInit.LiveKitService.ConnectedParticipants(ctx, s.Room)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range people {
		if !livekitInit.IsPerson(p) {
			continue
		}
		var allowed []string
		runs := false
		if q := s.Person(p.Identity); q != nil {
			allowed, runs = q.Allowed, model.RunsCall(q.Role)
		}
		changed, err := giveSources(ctx, s.Room, p, liveKitBusiness.PublishSources(held, allowed, runs))
		if err != nil {
			return n, err
		}
		if changed {
			n++
		}
	}
	var meta any
	if len(held) > 0 {
		meta = held
	}
	if err := setMeta(ctx, s.Room, metaHeld, meta); errors.Is(err, livekitInit.ErrRoomNotFound) {
		// Nobody is in this room now (everyone is in the call's other rooms):
		// its record holds, and there are no screens to tell.
		return n, nil
	} else if err != nil {
		return n, err
	}
	t := "held"
	if !on {
		t = "unheld"
	}
	tell(ctx, s.Room, liveKitBusiness.CallPacket{T: t, By: me.Name, Source: what})
	// Sharing held takes a board off the stage as it unpublishes a share.
	after := *s
	after.Held = held
	roomLocks.Acquire(s.Room, 0)
	defer roomLocks.Release(s.Room)
	if err := settleStage(ctx, &after); err != nil {
		// The hold stands; the board is settled again at the next change.
		helpers.LogErrorWithContext(ctx, "business/CallControl could not settle the board on %s's stage after a hold: %+v", s.Room, err)
	}
	return n, nil
}

// mayDo is whether someone in the call may do what although it may be held:
// it isn't, they run the call, or they were let through. Pure.
func mayDo(s *callStateBusiness.Session, p *callStateBusiness.Person, what string) bool {
	return !slices.Contains(s.Held, what) || model.RunsCall(p.Role) || slices.Contains(p.Allowed, what)
}

// allow lets one person do what (one of model.Holds) although the call holds
// it, or with on false takes that back: their record, what they may publish,
// and what the room sees of them.
func allow(ctx context.Context, s *callStateBusiness.Session, target *callStateBusiness.Person, what string, on bool) error {
	allowed, err := callStateBusiness.SetAllowed(ctx, s, target.Identity, what, on)
	if errors.Is(err, callStateBusiness.ErrNotInThisCall) {
		return ErrTargetGone
	}
	if err = settle(err); err != nil {
		return err
	}
	p, err := livekitInit.LiveKitService.Participant(ctx, s.Room, target.Identity)
	if err != nil {
		return err
	}
	if p == nil {
		return ErrTargetGone
	}
	if _, err := giveSources(ctx, s.Room, p, liveKitBusiness.PublishSources(s.Held, allowed, model.RunsCall(target.Role))); err != nil {
		return err
	}
	tellAttrs(s.Room, target.Identity, map[string]string{AttrAllowed: strings.Join(allowed, ",")})
	if what != model.HoldShare || on {
		return nil
	}
	// Sharing taken back while it is held takes their board off the stage.
	after := *s
	after.People = slices.Clone(s.People)
	for i := range after.People {
		if after.People[i].Identity == target.Identity {
			after.People[i].Allowed = allowed
		}
	}
	roomLocks.Acquire(s.Room, 0)
	defer roomLocks.Release(s.Room)
	return settleStage(ctx, &after)
}

// giveSources lets someone in the call publish just sources, unless they
// already may; answers whether it changed. The call server unpublishes a live
// track whose source is taken.
func giveSources(ctx context.Context, room string, p *livekit.ParticipantInfo, sources []string) (bool, error) {
	if livekitInit.PublishesOnly(p, sources) {
		return false, nil
	}
	return true, livekitInit.LiveKitService.SetPublishSources(ctx, room, p.Identity, sources)
}

// liveCall is a call by its session id while it goes on: ErrNotInCall for no
// such call, ErrCallOver for one that has ended. What present and
// inCallOrItsRooms start from.
func liveCall(ctx context.Context, sessionID string) (*callStateBusiness.Session, error) {
	s, err := callStateBusiness.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, ErrNotInCall
	}
	if !s.Live {
		return nil, ErrCallOver
	}
	return s, nil
}

// present is a live call and the person in it now, as both Call State and
// LiveKit see them: what every action starts from.
func present(ctx context.Context, sessionID, identity string) (*callStateBusiness.Session, *callStateBusiness.Person, error) {
	s, err := liveCall(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	me, err := hereNow(ctx, s, identity)
	if err != nil {
		return nil, nil, err
	}
	return s, me, nil
}

// hereNow is someone in a live call now, as both Call State and LiveKit see
// them. Call State hears of a leave from LiveKit's webhook, which can come
// late or not at all, so LiveKit's own list has the last word.
func hereNow(ctx context.Context, s *callStateBusiness.Session, identity string) (*callStateBusiness.Person, error) {
	me := s.Person(identity)
	if me == nil || !me.In {
		return nil, ErrNotInCall
	}
	here, err := livekitInit.LiveKitService.RoomHasParticipant(ctx, s.Room, identity)
	if err != nil {
		return nil, err
	}
	if !here {
		return nil, ErrNotInCall
	}
	return me, nil
}

// ActOnYourself does what anyone in a call may do to themselves, guests
// included: raise or lower their own hand, set or clear their feedback, and
// choose a breakout room (or the main room).
// identity is who the caller proved to be: a member's graph uid, or a guest's
// LiveKit identity from their own call token. Never through an API token.
func ActOnYourself(ctx context.Context, identity, sessionID string, a Action) (Result, error) {
	if helpers.ActsThroughToken(ctx) {
		return Result{}, ErrThroughToken
	}
	if !OwnAction(a, identity) {
		return Result{}, ErrUnknownAction
	}
	if a.Action == ActBreakoutChoose {
		return chooseRoom(ctx, identity, sessionID, a.Room)
	}
	s, _, err := present(ctx, sessionID, identity)
	if err != nil {
		return Result{}, err
	}
	switch a.Action {
	case ActRaiseHand, ActLowerHand:
		raised := a.Action == ActRaiseHand
		changed, at, err := callStateBusiness.SetHand(ctx, s, identity, raised)
		if err = settle(err); err != nil {
			return Result{}, err
		}
		tellAttrs(s.Room, identity, map[string]string{AttrHand: handValue(at)})
		if !changed {
			return Result{}, nil
		}
		return Result{Affected: 1}, nil
	case ActSetFeedback:
		changed, err := callStateBusiness.SetFeedback(ctx, s, identity, a.Feedback)
		if errors.Is(err, callStateBusiness.ErrNoSuchFeedback) {
			return Result{}, ErrNoSuchFeedback
		}
		if err = settle(err); err != nil {
			return Result{}, err
		}
		tellAttrs(s.Room, identity, map[string]string{AttrFeedback: a.Feedback})
		if !changed {
			return Result{}, nil
		}
		return Result{Affected: 1}, nil
	}
	return Result{}, ErrUnknownAction
}

// handValue is a raised hand's attribute: when it went up, in Unix
// milliseconds, or "" for a hand that is down. Pure.
func handValue(at *time.Time) string {
	if at == nil {
		return ""
	}
	return strconv.FormatInt(at.UnixMilli(), 10)
}

// targetOf is the person an action is done to, if it may be done to them.
func targetOf(s *callStateBusiness.Session, me *callStateBusiness.Person, actorIdentity string, a Action) (*callStateBusiness.Person, error) {
	if a.Identity == "" {
		return nil, ErrNoTarget
	}
	if a.Identity == actorIdentity && a.Action != ActSpotlight {
		return nil, ErrYourself
	}
	target := s.Person(a.Identity)
	if target == nil || !target.In {
		return nil, ErrTargetGone
	}
	switch a.Action {
	case ActRemove, ActMakeCohost, ActRemoveCohost:
		// Only on someone below you: a co-host can't put a host out.
		if rank(target.Role) >= rank(me.Role) {
			return nil, ErrAboveYou
		}
		if a.Action == ActMakeCohost && target.Guest {
			return nil, ErrGuestCohost
		}
	case ActMute, ActAskUnmute:
		if rank(target.Role) > rank(me.Role) {
			return nil, ErrAboveYou
		}
	}
	return target, nil
}

// settle turns "the call is over" into its refusal. Pure.
func settle(err error) error {
	if errors.Is(err, callStateBusiness.ErrCallOver) {
		return ErrCallOver
	}
	return err
}

// mute turns off someone's tracks of the given sources, answering how many
// were playing.
func mute(ctx context.Context, room, identity string, sources []livekit.TrackSource) (int, error) {
	p, err := livekitInit.LiveKitService.Participant(ctx, room, identity)
	if err != nil {
		return 0, err
	}
	if p == nil {
		return 0, ErrTargetGone
	}
	n := 0
	for _, t := range p.Tracks {
		if t == nil || t.Muted || !slices.Contains(sources, t.Source) {
			continue
		}
		if err := livekitInit.LiveKitService.MuteTrack(ctx, room, identity, t.Sid); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// others is everyone in the call now but you and, where the call has hosts,
// the people running it: whom "everyone" means for mute all and ask all.
func others(ctx context.Context, s *callStateBusiness.Session, me *callStateBusiness.Person) ([]*livekit.ParticipantInfo, error) {
	people, err := livekitInit.LiveKitService.ConnectedParticipants(ctx, s.Room)
	if err != nil {
		return nil, err
	}
	var out []*livekit.ParticipantInfo
	for _, p := range people {
		if !livekitInit.IsPerson(p) || p.Identity == me.Identity {
			continue
		}
		if !equalCall(s.Resource.Kind) {
			if q := s.Person(p.Identity); q != nil && (q.Role == model.RoleHost || q.Role == model.RoleCohost) {
				continue
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// muteAll turns off every microphone in the call but yours and the hosts'.
func muteAll(ctx context.Context, s *callStateBusiness.Session, me *callStateBusiness.Person) (Result, error) {
	people, err := others(ctx, s, me)
	if err != nil {
		return Result{}, err
	}
	var muted []string
	for _, p := range people {
		n, err := mute(ctx, s.Room, p.Identity, sourcesOf("microphone"))
		if errors.Is(err, ErrTargetGone) {
			continue
		}
		if err != nil {
			return Result{Affected: len(muted)}, err
		}
		if n > 0 {
			muted = append(muted, p.Identity)
		}
	}
	if len(muted) > 0 {
		tell(ctx, s.Room, liveKitBusiness.CallPacket{T: "muted", By: me.Name, Source: "microphone", Reason: "all"}, muted...)
	}
	return Result{Affected: len(muted)}, nil
}

// askAllToUnmute asks everyone whose microphone is off (but you and the
// hosts) to turn it on. Each chooses.
func askAllToUnmute(ctx context.Context, s *callStateBusiness.Session, me *callStateBusiness.Person) (Result, error) {
	people, err := others(ctx, s, me)
	if err != nil {
		return Result{}, err
	}
	var asked []string
	for _, p := range people {
		if !micOn(p) {
			asked = append(asked, p.Identity)
		}
	}
	if len(asked) > 0 {
		tell(ctx, s.Room, liveKitBusiness.CallPacket{T: "ask_unmute", By: me.Name, Reason: "all"}, asked...)
	}
	return Result{Affected: len(asked)}, nil
}

// micOn reports whether someone's microphone is playing. Pure.
func micOn(p *livekit.ParticipantInfo) bool {
	for _, t := range p.Tracks {
		if t != nil && t.Source == livekit.TrackSource_MICROPHONE && !t.Muted {
			return true
		}
	}
	return false
}

// tell sends a packet to people in the call, or everyone; best-effort, since
// what it explains has happened (or is about to) either way.
func tell(ctx context.Context, room string, p liveKitBusiness.CallPacket, identities ...string) {
	if err := liveKitBusiness.TellInCall(ctx, room, p, identities...); err != nil {
		helpers.LogErrorWithContext(ctx, "business/CallControl could not tell the call in %s (%s): %+v", room, p.T, err)
	}
}

// setMeta changes one of the call's settings where every screen reads it; a
// nil value takes the setting away.
func setMeta(ctx context.Context, room, key string, value any) error {
	_, err := livekitInit.LiveKitService.UpdateRoomMetadata(ctx, room, map[string]any{key: value})
	return err
}
