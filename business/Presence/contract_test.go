package business

import (
	"context"
	"errors"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Presence is today's fields: a connected device, the status chosen, the
// emoji status set, and the held status the push gate holds by.
func TestPresenceIsTodaysFields(t *testing.T) {
	two, none := 2, 0
	until := time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC)
	set := &dgraphStruct.DgraphUserStatusEmoji{EmojiUuid: "palm_tree", EmojiDesc: "On holiday", ExpiryAt: &until}
	s := stateOf(&dgraphStruct.DgraphUser{DevicesConnected: &two, Status: dgraphStruct.USER_OPT_STATUS_ONLINE, StatusEmoji: []*dgraphStruct.DgraphUserStatusEmoji{set}},
		notificationBusiness.HeldStatus{Until: &until, Reason: "focus"})
	if !s.Connected || s.Status != "online" || s.HeldUntil == nil || !s.HeldUntil.Equal(until) || s.HeldReason != "focus" {
		t.Errorf("%+v", s)
	}
	if s.Emoji != "palm_tree" || s.Text != "On holiday" || s.ClearAt == nil || !s.ClearAt.Equal(until) {
		t.Errorf("the emoji status: %+v", s)
	}
	for _, n := range []*int{&none, nil} {
		if s := stateOf(&dgraphStruct.DgraphUser{DevicesConnected: n}, notificationBusiness.HeldStatus{}); s.Connected || s.HeldUntil != nil || s.Emoji != "" || s.ClearAt != nil {
			t.Errorf("no device and no status: %+v", s)
		}
	}
	if _, err := People.State(context.Background(), "not a uuid"); !errors.Is(err, ErrNotFound) {
		t.Errorf("not a person: %v", err)
	}
}
