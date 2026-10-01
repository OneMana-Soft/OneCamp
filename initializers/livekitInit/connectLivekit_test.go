package livekitInit

import (
	"errors"
	"testing"
)

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
