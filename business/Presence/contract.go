// Package business (Presence) is presence and status
// (design/chat-vs-slack/PROGRAM.md §4.2 #27; A7 builds it): one answer to "is
// this person here, and can they be reached", for every avatar, hover card,
// mention and the router's active device.
//
// DAY 0. Today's fields, read as today's readers read them: a connected device
// (Dgraph user_device_connected, kept by the broker's connect events), the
// status they chose (user_status) and the emoji status they set while it lasts
// (user_emoji_statuses), and whether their notifications are held and until
// when (business/Notification HeldStatusOf: a pause, focus time or quiet
// hours, the rules the push gate holds by). Nothing calls People yet, so
// nothing changes by its existing. A7 adds away, calls, out of office and the
// calendar's status (calendar.StatusFor) behind the same name.
package business

import (
	"context"
	"errors"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// State is someone's presence now.
type State struct {
	// Connected is whether a device of theirs is connected.
	Connected bool `json:"connected"`
	// Status is the status they chose: "online" or "offline" ("" for none).
	Status string `json:"status,omitempty"`
	// Emoji and Text are the status they set, and ClearAt when it ends: the
	// one that lasts longest of those not ended yet. Empty when they set none.
	Emoji   string     `json:"emoji,omitempty"`
	Text    string     `json:"text,omitempty"`
	ClearAt *time.Time `json:"clear_at,omitempty"`
	// HeldUntil is when their notifications stop being held, and HeldReason
	// why ("paused", "focus" or "quiet_hours"); nil when nothing holds them.
	HeldUntil  *time.Time `json:"held_until,omitempty"`
	HeldReason string     `json:"held_reason,omitempty"`
}

// Tracker answers presence.
type Tracker interface {
	State(ctx context.Context, userUUID string) (State, error)
}

// ErrNotFound is someone who isn't there.
var ErrNotFound = errors.New("presence: no such person")

// People is the presence everything asks.
var People Tracker = todaysPresence{}

type todaysPresence struct{}

func (todaysPresence) State(ctx context.Context, userUUID string) (State, error) {
	id, err := uuid.Parse(userUUID)
	if err != nil {
		return State{}, ErrNotFound
	}
	user, err := userBusiness.GetDgraphUserInfoByUUID(ctx, id.String())
	if err != nil {
		return State{}, err
	}
	if user == nil || user.Uuid == "" {
		return State{}, ErrNotFound
	}
	held, err := notificationBusiness.HeldStatusOf(id, time.Now())
	if err != nil {
		return State{}, err
	}
	return stateOf(user, held), nil
}

// stateOf is a person's presence from their user as read and their held
// status. Pure.
func stateOf(user *dgraphStruct.DgraphUser, held notificationBusiness.HeldStatus) State {
	s := State{Status: user.Status, HeldUntil: held.Until, HeldReason: held.Reason}
	s.Connected = user.DevicesConnected != nil && *user.DevicesConnected > 0
	// The query keeps only the status that ends last of those not ended yet.
	if len(user.StatusEmoji) > 0 && user.StatusEmoji[0] != nil {
		e := user.StatusEmoji[0]
		s.Emoji, s.Text, s.ClearAt = e.EmojiUuid, e.EmojiDesc, e.ExpiryAt
	}
	return s
}
