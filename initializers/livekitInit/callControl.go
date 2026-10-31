package livekitInit

// What the people running a call do to it, as LiveKit's room service does it
// (business/CallControl decides who may).

import (
	"context"

	"github.com/livekit/protocol/livekit"
)

// Participant answers one person connected to a room, with their tracks, or
// nil when they aren't in it.
func (s *LiveKitServiceStruct) Participant(ctx context.Context, room, identity string) (*livekit.ParticipantInfo, error) {
	p, err := s.LiveKitClient.GetParticipant(ctx, &livekit.RoomParticipantIdentity{Room: room, Identity: identity})
	if err != nil {
		if isRoomNotFoundErr(err) {
			return nil, nil
		}
		return nil, err
	}
	if p == nil || p.State == livekit.ParticipantInfo_DISCONNECTED {
		return nil, nil
	}
	return p, nil
}

// MuteTrack mutes one of someone's published tracks. Only ever mutes: the
// server never turns anyone's microphone or camera on. They are asked, and
// they choose.
func (s *LiveKitServiceStruct) MuteTrack(ctx context.Context, room, identity, trackSID string) error {
	_, err := s.LiveKitClient.MutePublishedTrack(ctx, &livekit.MuteRoomTrackRequest{
		Room: room, Identity: identity, TrackSid: trackSID, Muted: true,
	})
	if err != nil && isRoomNotFoundErr(err) {
		// Gone already, or the track ended: it is not playing, which is the point.
		return nil
	}
	return err
}

// SetPublishSources changes what someone already in a call may publish
// (some of CallPublishSources), as a token minted now would grant it. The
// call server unpublishes a live track whose source is taken away.
func (s *LiveKitServiceStruct) SetPublishSources(ctx context.Context, room, identity string, sources []string) error {
	_, err := s.LiveKitClient.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
		Room: room, Identity: identity, Permission: callGrant(room, false, sources).ToPermission(),
	})
	if err != nil && isRoomNotFoundErr(err) {
		return nil
	}
	return err
}

// PublishesOnly is whether someone in a call may already publish just
// sources, as SetPublishSources would leave them. Pure.
func PublishesOnly(p *livekit.ParticipantInfo, sources []string) bool {
	// The room plays no part in a permission.
	return p != nil && callGrant("", false, sources).MatchesPermission(p.Permission)
}

// SetAttributes sets some of a participant's attributes, which everyone in the
// room sees; the others stay as they are. Only the server can: tokens never
// grant a participant the right to change their own.
func (s *LiveKitServiceStruct) SetAttributes(ctx context.Context, room, identity string, attrs map[string]string) error {
	_, err := s.LiveKitClient.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
		Room: room, Identity: identity, Attributes: attrs,
	})
	if err != nil && isRoomNotFoundErr(err) {
		return nil
	}
	return err
}
