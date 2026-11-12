package livekitInit

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	lka "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
)

// The recorder's token (H3, meet-17): the one the recording stage's entrance
// hands the web's layout page. It joins only its room, hidden, as a recorder
// that subscribes and never publishes, and only for as long as connecting
// takes. LiveKit lists whoever holds it as an egress, so it is no one in the
// call: nobody is rung, listed or kept in a call by it.
func TestRecorderToken(t *testing.T) {
	const secret = "a LiveKit secret of thirty-two characters"
	s := &LiveKitServiceStruct{Config: &LiveKitConfigStruct{HostURL: "http://livekit:7880", ApiKey: "key", ApiSecret: secret}}
	raw, err := s.RecorderToken("room-1")
	if err != nil {
		t.Fatal(err)
	}
	v, err := lka.ParseAPIToken(raw)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := v.Verify(secret)
	if err != nil {
		t.Fatal(err)
	}
	g := claims.Video
	if g == nil || !g.RoomJoin || g.Room != "room-1" || !g.Hidden || !g.Recorder || g.RoomAdmin || g.RoomCreate || g.RoomList || g.RoomRecord {
		t.Fatalf("the recorder's grant: %+v, want joining room-1 alone, hidden, as a recorder", g)
	}
	if !g.GetCanSubscribe() || g.GetCanPublish() || g.GetCanPublishData() || g.GetCanUpdateOwnMetadata() {
		t.Errorf("the recorder subscribes %v, publishes %v, sends data %v, renames itself %v: want subscribing alone",
			g.GetCanSubscribe(), g.GetCanPublish(), g.GetCanPublishData(), g.GetCanUpdateOwnMetadata())
	}
	if claims.GetParticipantKind() != livekit.ParticipantInfo_EGRESS {
		t.Errorf("the recorder joins as %v, want an egress", claims.GetParticipantKind())
	}
	if claims.Identity == "" || claims.Name != "" {
		t.Errorf("the recorder is %q named %q: want an identity of its own and no name", claims.Identity, claims.Name)
	}
	if again, _ := s.RecorderToken("room-1"); again != "" {
		if v2, err := lka.ParseAPIToken(again); err == nil && v2.Identity() == claims.Identity {
			t.Errorf("two recorders share the identity %q: LiveKit would put the first out", claims.Identity)
		}
	}
	part := strings.Split(raw, ".")
	body, err := base64.RawURLEncoding.DecodeString(part[1])
	if err != nil {
		t.Fatal(err)
	}
	var times struct{ Exp, Nbf int64 }
	if err := json.Unmarshal(body, &times); err != nil {
		t.Fatal(err)
	}
	if life := time.Duration(times.Exp-time.Now().Unix()) * time.Second; life <= 0 || life > RecorderTokenTTL || RecorderTokenTTL > 2*time.Minute {
		t.Errorf("the recorder's token lasts %v (RecorderTokenTTL %v), want at most two minutes", life, RecorderTokenTTL)
	}
	who := &livekit.ParticipantInfo{Identity: claims.Identity, Kind: claims.GetParticipantKind(), Permission: &livekit.ParticipantPermission{Hidden: g.Hidden}}
	if IsPerson(who) {
		t.Error("the recorder counts as a person in the call")
	}
}

// The recorder dials the LiveKit server this one talks to, as a websocket:
// the address egress itself dials on a stock install (ws://livekit:7880).
func TestRecorderServerURL(t *testing.T) {
	for _, c := range []struct{ host, want string }{
		{"http://livekit:7880", "ws://livekit:7880"},
		{"https://lk.example.test", "wss://lk.example.test"},
		{"wss://lk.example.test", "wss://lk.example.test"},
		{"", ""},
	} {
		s := &LiveKitServiceStruct{Config: &LiveKitConfigStruct{HostURL: c.host}}
		if got := s.RecorderServerURL(); got != c.want {
			t.Errorf("RecorderServerURL for %q = %q, want %q", c.host, got, c.want)
		}
	}
	if got := (&LiveKitServiceStruct{}).RecorderServerURL(); got != "" {
		t.Errorf("RecorderServerURL with no LiveKit = %q, want none", got)
	}
}

// LiveKit's reason an egress ended never carries the recording stage's
// ticket (its address's path) nor the recorder's token (the layout page's
// fragment, or egress's own query).
func TestEgressReasonCutsTheStageTicket(t *testing.T) {
	for _, c := range []struct{ name, reason, want string }{
		{"the entrance", "page load failed at https://api.example.test/recording-stage/eyJyIjoicm9vbSJ9.c2ln?layout=&url=ws%3A%2F%2Flivekit%3A7880&token=eyJ.x.y (3 tries)",
			"page load failed at https://api.example.test/recording-stage/...?... (3 tries)"},
		{"the entrance alone", "net::ERR_CONNECTION_REFUSED at https://api.example.test/recording-stage/eyJyIjoicm9vbSJ9.c2ln",
			"net::ERR_CONNECTION_REFUSED at https://api.example.test/recording-stage/..."},
		{"the page's fragment", "START_RECORDING never logged at https://team.example.test/call-recording-layout#lk=ws%3A%2F%2Flivekit%3A7880&t=eyJ.x.y",
			"START_RECORDING never logged at https://team.example.test/call-recording-layout?..."},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := EgressReason(&livekit.EgressInfo{Error: c.reason}); got != c.want {
				t.Errorf("EgressReason = %q, want %q", got, c.want)
			}
		})
	}
}
