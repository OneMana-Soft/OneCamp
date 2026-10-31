package business

// What the people running a call hold for everyone else (calls meet-12,
// zoom-14): sharing, the microphone and the camera are LiveKit's to enforce,
// by what each person may publish. A token is minted with it, and
// business/CallControl changes it for the people already in the call. Chat's
// hold is business/Conversation's, and reactions' every screen's.

import (
	"context"
	"slices"

	"github.com/akashc777/OneCamp/initializers/livekitInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
)

// heldSources is what each hold on media takes from what someone may publish.
var heldSources = map[string][]string{
	callStateModel.HoldMicrophone: {"microphone"},
	callStateModel.HoldCamera:     {"camera"},
	callStateModel.HoldShare:      {"screen_share", "screen_share_audio"},
}

// PublishSources is what someone may publish in a call: everything, less what
// the people running it hold for everyone else (held), unless they run it
// (runs) or were let through (allowed). Pure.
func PublishSources(held, allowed []string, runs bool) []string {
	out := make([]string, 0, len(livekitInit.CallPublishSources))
	for _, source := range livekitInit.CallPublishSources {
		if runs || !withheld(source, held, allowed) {
			out = append(out, source)
		}
	}
	return out
}

// withheld is whether a hold takes source from someone. Pure.
func withheld(source string, held, allowed []string) bool {
	for hold, sources := range heldSources {
		if slices.Contains(sources, source) && slices.Contains(held, hold) && !slices.Contains(allowed, hold) {
			return true
		}
	}
	return false
}

// SourcesAtDoor is what someone may publish in the call going on in a room,
// as their token is minted: user is a member, nil for a guest (who never runs
// a call), and meetingHost the instant meeting's own host. No call yet, or no
// Call State at all, holds nothing. A record that doesn't answer is
// ErrCallUnchecked: no token is given without it.
func SourcesAtDoor(ctx context.Context, room string, user *dgraphStruct.DgraphUser, meetingHost bool) ([]string, error) {
	s, err := callStateModel.OpenSessionOf(ctx, room)
	if noRecord(err) {
		return livekitInit.CallPublishSources, nil
	}
	if err != nil {
		return nil, unchecked(ctx, room, err)
	}
	if s == nil || len(s.Held) == 0 || meetingHost {
		return livekitInit.CallPublishSources, nil
	}
	if user == nil || user.Uid == "" {
		return PublishSources(s.Held, nil, false), nil
	}
	a, err := callStateModel.AttendeeOf(ctx, s.ID, user.Uid)
	if err != nil {
		return nil, unchecked(ctx, room, err)
	}
	if a != nil {
		return PublishSources(s.Held, a.Allowed, callStateModel.RunsCall(a.Role)), nil
	}
	// Not in this call before: the part they will take by who they are.
	host, err := HostByRule(ctx, room, user, IsWorkspaceAdmin(ctx, user.Uuid))
	if err != nil {
		return nil, unchecked(ctx, room, err)
	}
	return PublishSources(s.Held, nil, host), nil
}
