package business

// Doing what someone running a call asks: mute, mute everyone, ask to
// unmute, remove, lock, end for everyone, co-hosts, spotlight, people joining
// muted.

import (
	"context"
	"errors"
	"slices"
	"time"

	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
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
	need := abilityFor(a.Action)
	if need == "" {
		return Result{}, ErrUnknownAction
	}
	if helpers.ActsThroughToken(ctx) {
		return Result{}, ErrThroughToken
	}
	if actor == nil || actor.Uid == "" {
		return Result{}, ErrNotInCall
	}
	s, err := callStateBusiness.Get(ctx, sessionID)
	if err != nil {
		return Result{}, err
	}
	if s == nil {
		return Result{}, ErrNotInCall
	}
	if !s.Live {
		return Result{}, ErrCallOver
	}
	me := s.Person(actor.Uid)
	if me == nil || !me.In {
		return Result{}, ErrNotInCall
	}
	here, err := livekitInit.LiveKitService.RoomHasParticipant(ctx, s.Room, actor.Uid)
	if err != nil {
		return Result{}, err
	}
	if !here {
		return Result{}, ErrNotInCall
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
		tell(ctx, s.Room, liveKitBusiness.CallPacket{T: "ask_unmute", By: me.Name}, target.Identity)
		return Result{Affected: 1}, nil
	case ActAskAllUnmute:
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
	case ActMuteOnEntry:
		if err := settle(callStateBusiness.SetMuteOnEntry(ctx, s, a.On)); err != nil {
			return Result{}, err
		}
		var meta any
		if a.On {
			meta = true
		}
		return Result{Affected: 1}, setMeta(ctx, s.Room, metaMuteOnEntry, meta)
	}
	return Result{}, ErrUnknownAction
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
		if !human(p) || p.Identity == me.Identity {
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
