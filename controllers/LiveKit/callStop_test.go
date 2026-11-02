package controllers

import (
	"reflect"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// A call's end clears the call dot of the conversation it was in, and only
// that one. A breakout room's name leads back to its call's conversation
// (RefOfRoom answers a breakout with its call's ref), so a breakout ending
// while its call goes on must clear nothing: the call's dot stays lit.
func TestACallsEndClearsOnlyItsOwnConversationsDot(t *testing.T) {
	var published []string
	chatStop, channelStop := publishChatStop, publishChannelStop
	t.Cleanup(func() { publishChatStop, publishChannelStop = chatStop, channelStop })
	publishChatStop = func(id string) { published = append(published, "chat "+id) }
	publishChannelStop = func(id string) { published = append(published, "channel "+id) }

	channel := uuid.New().String()
	dm := helpers.GetGroupingId(uuid.New().String(), uuid.New().String())
	group := "abcdef0123456789abcdef0123456789"
	meeting := "meet-" + uuid.New().String()

	cases := []struct {
		name string
		room string
		want []string
	}{
		{"a channel call's breakout clears nothing", channel + "~b1", nil},
		{"a meeting's breakout clears nothing", meeting + "~b2", nil},
		{"a channel's call clears its channel", channel, []string{"channel " + channel}},
		{"a DM's call clears its chat", dm, []string{"chat " + dm}},
		{"a group's call clears its chat", group, []string{"chat " + group}},
		{"a meeting's call clears nothing (no conversation holds it)", meeting, nil},
		{"a name no call uses clears nothing", "not-a-call-room", nil},
		{"an empty name clears nothing", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			published = nil
			broadcastCallStop(tc.room)
			if !reflect.DeepEqual(published, tc.want) {
				t.Fatalf("broadcastCallStop(%q) published %q, want %q", tc.room, published, tc.want)
			}
		})
	}
}
