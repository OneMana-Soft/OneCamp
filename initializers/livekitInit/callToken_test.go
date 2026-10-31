package livekitInit

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	lka "github.com/livekit/protocol/auth"
)

const testKey, testSecret = "test-key", "test-secret-long-enough-to-sign-with-it"

func testService() *LiveKitServiceStruct {
	return &LiveKitServiceStruct{Config: &LiveKitConfigStruct{ApiKey: testKey, ApiSecret: testSecret}}
}

func claims(t *testing.T, token string) *lka.ClaimGrants {
	t.Helper()
	v, err := lka.ParseAPIToken(token)
	if err != nil {
		t.Fatal(err)
	}
	c, err := v.Verify(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// CALL-BUG-1. Someone who joins with their microphone or camera off can still
// turn it on in the call. The token used to grant "microphone" and "camera"
// only when they were on in the green room, and LiveKit then refused every
// later Unmute ("failed to publish track, insufficient permissions"), for
// members, guests and the host of an instant meeting alike.
func TestEveryCallTokenLetsPeoplePublishTheirMicCameraAndScreen(t *testing.T) {
	s := testService()
	member, err := s.GenerateToken("room-1", "0x1", "Maya", false, "", CallPublishSources)
	if err != nil {
		t.Fatal(err)
	}
	host, err := s.GenerateToken("meet-1", "0x2", "Sam", true, "", CallPublishSources)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := s.GenerateGuestToken("meet-1", "guest-x", "Guest: Ann", "", CallPublishSources)
	if err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{"member": member, "instant host": host, "guest": guest} {
		c := claims(t, token)
		if c.Video == nil || !c.Video.GetCanPublish() || !c.Video.GetCanSubscribe() || !c.Video.GetCanPublishData() {
			t.Fatalf("%s: grant %+v", name, c.Video)
		}
		for _, source := range []string{"camera", "microphone", "screen_share", "screen_share_audio"} {
			if !slices.Contains(c.Video.CanPublishSources, source) {
				t.Errorf("%s: the token does not let them publish %q, so they can never turn it on: %v", name, source, c.Video.CanPublishSources)
			}
		}
		if c.Video.GetCanUpdateOwnMetadata() {
			t.Errorf("%s: a participant must not rename themselves in a call", name)
		}
	}
	if !claims(t, host).Video.RoomAdmin || claims(t, member).Video.RoomAdmin || claims(t, guest).Video.RoomAdmin {
		t.Error("room admin is the instant meeting's host's alone")
	}
}

// What the people running a call hold for everyone else (calls meet-12,
// zoom-14) narrows what a token, or a permission given to someone already in
// the call, lets them publish. None at all is no publishing: LiveKit reads an
// empty list as every source. Data and listening are never taken.
func TestAHeldSourceIsLeftOutOfTheGrant(t *testing.T) {
	s := testService()
	narrowed, err := s.GenerateToken("room-1", "0x1", "Maya", false, "", []string{"camera"})
	if err != nil {
		t.Fatal(err)
	}
	c := claims(t, narrowed).Video
	if !c.GetCanPublish() || !slices.Equal(c.CanPublishSources, []string{"camera"}) {
		t.Errorf("a camera-only grant: %+v", c)
	}
	none, err := s.GenerateGuestToken("meet-1", "guest-x", "Guest: Ann", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := claims(t, none).Video; c.GetCanPublish() || !c.GetCanPublishData() || !c.GetCanSubscribe() {
		t.Errorf("a grant of nothing to publish: %+v", c)
	}
	for _, sources := range [][]string{nil, {"microphone"}} {
		p := callGrant("room-1", false, sources).ToPermission()
		if p.CanPublish != (len(sources) > 0) || len(p.CanPublishSources) != len(sources) || !p.CanPublishData || !p.CanSubscribe || p.CanUpdateMetadata {
			t.Errorf("the permission for %v: %+v", sources, p)
		}
	}
}

// CALL-BUG-11. A token outlives the right it was minted for (removed from the
// channel, a guest link revoked): members' lasted a day and guests' four
// hours. LiveKit refreshes a connected participant's token itself, so a short
// one bounds only the window to connect.
func TestCallTokensAreShortLived(t *testing.T) {
	s := testService()
	member, _ := s.GenerateToken("room-1", "0x1", "Maya", false, "", CallPublishSources)
	guest, _ := s.GenerateGuestToken("meet-1", "guest-x", "Guest: Ann", "", CallPublishSources)
	for name, token := range map[string]string{"member": member, "guest": guest} {
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			t.Fatalf("%s: not a JWT", name)
		}
		raw, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Exp int64 `json:"exp"`
			Nbf int64 `json:"nbf"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		if life := time.Duration(c.Exp-c.Nbf) * time.Second; life <= 0 || life > 10*time.Minute {
			t.Errorf("%s: the token lives %s, want at most 10m", name, life)
		}
	}
}

// The language a person speaks travels in their token for the server's
// transcriber, and only a real language tag does.
func TestCallTokenCarriesTheSpeakersLanguage(t *testing.T) {
	s := testService()
	for raw, want := range map[string]string{
		"en-US":                 "en-US",
		" hi ":                  "hi",
		"zh-Hant-TW":            "zh-Hant-TW",
		"":                      "",
		"en_US":                 "",
		"<script>":              "",
		"aaaa-bbbbbbbbbbbbbbbb": "",
	} {
		token, err := s.GenerateToken("room-1", "0x1", "Maya", false, raw, CallPublishSources)
		if err != nil {
			t.Fatal(err)
		}
		if got := claims(t, token).Attributes[LanguageAttribute]; got != want {
			t.Errorf("language %q became %q, want %q", raw, got, want)
		}
	}
}

// CALL-BUG-8. The room is made when the first token is minted, before anyone
// connects, and it closed 5 seconds after that with nobody in it: a first
// person whose browser took longer to connect had the call end before it
// began.
func TestACallRoomWaitsForASlowFirstJoin(t *testing.T) {
	req := callRoomRequest("room-1")
	if req.EmptyTimeout < 60 {
		t.Errorf("an empty room closes after %ds; a slow first join takes longer", req.EmptyTimeout)
	}
	if req.DepartureTimeout < 10 {
		t.Errorf("a room closes %ds after its last person leaves; a dropped network takes longer to come back", req.DepartureTimeout)
	}
}
