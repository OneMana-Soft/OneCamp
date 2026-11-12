package business

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/livekit/protocol/livekit"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/livekitfake"
)

// meet-04. A companion is asked for by its person, signed in: never through
// an API token, which acts at most as a member and never runs a device of
// theirs in a call; and never for nobody.
func TestACompanionIsNeverThroughAnAPIToken(t *testing.T) {
	if _, err := CompanionToken(helpers.WithActsThroughToken(context.Background()), &dgraphStruct.DgraphUser{Uid: "0x2a"}, "5e550000-0000-4000-8000-0000000000c1", ""); !errors.Is(err, ErrThroughToken) {
		t.Errorf("a companion through an API token: %v, want ErrThroughToken", err)
	}
	if _, err := CompanionToken(context.Background(), nil, "5e550000-0000-4000-8000-0000000000c1", ""); !errors.Is(err, ErrNotInCall) {
		t.Errorf("a companion for nobody: %v, want ErrNotInCall", err)
	}
}

// meet-04. What is done to someone in a call is done on every device of
// theirs (a hold, a mute, letting them through): their own connection and
// their companions', and nobody else's.
func TestWhatIsDoneToSomeoneReachesTheirCompanions(t *testing.T) {
	samPhone, err := livekitInit.NewCompanionIdentity("0x2b")
	if err != nil {
		t.Fatal(err)
	}
	mayaPhone, err := livekitInit.NewCompanionIdentity("0x2a")
	if err != nil {
		t.Fatal(err)
	}
	saved := connectedPeople
	t.Cleanup(func() { connectedPeople = saved })
	connectedPeople = func(context.Context, string) ([]*livekit.ParticipantInfo, error) {
		return []*livekit.ParticipantInfo{{Identity: "0x2b"}, {Identity: samPhone}, {Identity: "0x2a"}, {Identity: mayaPhone}, {Identity: "guest-x-1"}, {Identity: "0x2b~c~nothex000000"}}, nil
	}
	got, err := devicesOf(context.Background(), "room", "0x2b")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range got {
		ids = append(ids, p.Identity)
	}
	if want := []string{"0x2b", samPhone}; !slices.Equal(ids, want) {
		t.Errorf("Sam's devices: %v, want %v", ids, want)
	}
}

// meet-04. A mute of someone's screen reaches the screen they present from a
// companion, and nobody else's: Sam presenting from his phone, with or
// without his own connection in the call, is muted; Maya's phone isn't.
func TestAMuteReachesTheScreenACompanionPresents(t *testing.T) {
	const room = "c0ffee00-0000-4000-8000-0000000000c7"
	samPhone, err := livekitInit.NewCompanionIdentity("0x2b")
	if err != nil {
		t.Fatal(err)
	}
	mayaPhone, err := livekitInit.NewCompanionIdentity("0x2a")
	if err != nil {
		t.Fatal(err)
	}
	livekitfake.Serve(t, room, "RM_7")
	screen := func(sid string) []*livekit.TrackInfo {
		return []*livekit.TrackInfo{{Sid: sid, Source: livekit.TrackSource_SCREEN_SHARE}}
	}
	sam := &livekit.ParticipantInfo{Identity: "0x2b", Tracks: []*livekit.TrackInfo{{Sid: "TR_mic", Source: livekit.TrackSource_MICROPHONE}}}
	in := []*livekit.ParticipantInfo{sam, {Identity: samPhone, Tracks: screen("TR_sam")}, {Identity: "0x2a"}, {Identity: mayaPhone, Tracks: screen("TR_maya")}}
	saved := connectedPeople
	t.Cleanup(func() { connectedPeople = saved })
	connectedPeople = func(context.Context, string) ([]*livekit.ParticipantInfo, error) { return slices.Clone(in), nil }

	if n, err := mute(context.Background(), room, "0x2b", sourcesOf("screen_share")); err != nil || n != 1 {
		t.Errorf("muting Sam's screen, presented from his phone: %d tracks (%v), want 1", n, err)
	}
	in = in[1:]
	if n, err := mute(context.Background(), room, "0x2b", sourcesOf("screen_share")); err != nil || n != 1 {
		t.Errorf("muting Sam's screen, in the call only on his phone: %d tracks (%v), want 1", n, err)
	}
	if _, err := mute(context.Background(), room, "0x2c", sourcesOf("screen_share")); !errors.Is(err, ErrTargetGone) {
		t.Errorf("muting someone on no device: %v, want ErrTargetGone", err)
	}
}
