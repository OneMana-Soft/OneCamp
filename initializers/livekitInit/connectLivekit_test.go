package livekitInit

import (
	"errors"
	"testing"

	"github.com/livekit/protocol/livekit"
)

// V-89. One rule says who is a person in a call: the transcription agent, a
// recorder and a hidden watcher are not. They are never recorded in a call,
// never handed it, never muted with everyone, never keep it going, and never
// light its dot. Someone who dialled in is a person.
func TestOnlyPeopleCount(t *testing.T) {
	for _, c := range []struct {
		name string
		p    *livekit.ParticipantInfo
		want bool
	}{
		{"a member", &livekit.ParticipantInfo{Identity: "0x1"}, true},
		{"a guest", &livekit.ParticipantInfo{Identity: "guest-a-b"}, true},
		{"someone who dialled in", &livekit.ParticipantInfo{Identity: "sip_1", Kind: livekit.ParticipantInfo_SIP}, true},
		{"the transcription agent", &livekit.ParticipantInfo{Identity: "transcriber-bot"}, false},
		{"an agent", &livekit.ParticipantInfo{Identity: "agent-1", Kind: livekit.ParticipantInfo_AGENT}, false},
		{"a hidden recorder", &livekit.ParticipantInfo{Identity: "EG_1", Permission: &livekit.ParticipantPermission{Hidden: true}}, false},
		{"an egress", &livekit.ParticipantInfo{Identity: "EG_2", Kind: livekit.ParticipantInfo_EGRESS}, false},
		{"a hidden watcher", &livekit.ParticipantInfo{Identity: "watcher", Permission: &livekit.ParticipantPermission{Hidden: true}}, false},
		{"no identity", &livekit.ParticipantInfo{Identity: " "}, false},
		{"nobody", nil, false},
	} {
		if got := IsPerson(c.p); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsAgentIdentity(t *testing.T) {
	cases := []struct {
		identity string
		want     bool
	}{
		{"transcriber-bot", true},
		{"", false},
		{"user-123", false},
		{"Transcriber-Bot", false}, // case-sensitive: must match agent exactly
		{"transcriber-bot-2", false},
	}
	for _, c := range cases {
		if got := isAgentIdentity(c.identity); got != c.want {
			t.Errorf("isAgentIdentity(%q) = %v, want %v", c.identity, got, c.want)
		}
	}
}

func TestIsRoomNotFoundErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"not found", errors.New("twirp error not_found: room not found"), true},
		{"notfound no space", errors.New("NotFound"), true},
		{"does not exist", errors.New("room does not exist"), true},
		{"no such room", errors.New("no such room: abc"), true},
		{"case insensitive", errors.New("Room Not Found"), true},
		{"unrelated", errors.New("internal server error"), false},
		{"permission", errors.New("permission denied"), false},
	}
	for _, c := range cases {
		if got := isRoomNotFoundErr(c.err); got != c.want {
			t.Errorf("isRoomNotFoundErr(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
