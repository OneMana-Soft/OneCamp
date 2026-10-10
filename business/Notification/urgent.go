package notification

// Seeing that a teammate has paused notifications, and Slack's "notify
// anyway": one urgent ping a day, in a DM, that reaches their devices through
// a pause or quiet hours.

import (
	"context"
	"errors"
	"time"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	fcmDomain "github.com/akashc777/OneCamp/domain/UserFCMToken"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/google/uuid"
)

// HeldStatus is whether someone's notifications are held right now, until
// when, and why ("paused" or "quiet_hours"). Empty Reason: not held.
type HeldStatus struct {
	Until  *time.Time `json:"until,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

// HeldFor reads a preference row as a status at now: the same rules the
// dispatcher holds email by, so what a teammate sees is what happens. Pure.
func HeldFor(p *prefModels.UserNotificationPreference, now time.Time) HeldStatus {
	if p == nil {
		return HeldStatus{}
	}
	if p.NotificationsPausedUntil != nil && p.NotificationsPausedUntil.After(now) {
		until := *p.NotificationsPausedUntil
		return HeldStatus{Until: &until, Reason: "paused"}
	}
	if at, deferred := quietHoursDelay(now, p); deferred {
		return HeldStatus{Until: &at, Reason: "quiet_hours"}
	}
	return HeldStatus{}
}

// HeldStatusOf is a teammate's status, read without creating a row for them.
func HeldStatusOf(userID uuid.UUID, now time.Time) (HeldStatus, error) {
	p, err := prefModels.GetByUserID(userID)
	if err != nil {
		// No row is no preferences: nothing is held.
		return HeldStatus{}, nil
	}
	return HeldFor(p, now), nil
}

// UrgentError is a ping that can't be sent; its text is written for people.
type UrgentError struct{ Msg string }

func (e *UrgentError) Error() string { return e.Msg }

const urgentPerDay = 1

func countRecentPings(sender, recipient uuid.UUID, since time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	var n int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM urgent_pings WHERE sender_id = $1 AND recipient_id = $2 AND created_at > $3`,
		sender, recipient, since).Scan(&n)
	return n, err
}

func recordPing(sender, recipient uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`INSERT INTO urgent_pings (sender_id, recipient_id) VALUES ($1, $2)`, sender, recipient)
	return err
}

// NotifyAnyway sends one urgent ping from sender to a teammate they have a DM
// with, through a pause or quiet hours, at most once a day per pair.
func NotifyAnyway(ctx context.Context, sender *userModels.UserInfo, recipient uuid.UUID, now time.Time) error {
	if recipient == sender.UserPostgresInfo.Id {
		return &UrgentError{"You can't ping yourself."}
	}
	status, err := HeldStatusOf(recipient, now)
	if err != nil {
		return err
	}
	if status.Reason == "" {
		return &UrgentError{"Their notifications aren't paused; your message already reached them."}
	}
	grp := helpers.GetGroupingId(sender.UserDgraphInfo.Uuid, recipient.String())
	dm, err := chatDomain.GetDgraphDmBasicInfoFromDgraph(ctx, sender.UserDgraphInfo.Uid, grp)
	if err != nil || dm == nil || dm.ParticipantIsMember == 0 {
		return &UrgentError{"Message them first, then notify them if it's urgent."}
	}
	inDM := false
	for _, p := range dm.Participants {
		if p != nil && p.Uuid == recipient.String() {
			inDM = true
		}
	}
	if !inDM {
		return &UrgentError{"Message them first, then notify them if it's urgent."}
	}
	if n, err := countRecentPings(sender.UserPostgresInfo.Id, recipient, now.Add(-24*time.Hour)); err != nil {
		return err
	} else if n >= urgentPerDay {
		return &UrgentError{"You've already notified them today. They'll see your messages when they're back."}
	}
	tokens, err := fcmDomain.GetFCMTokenByUserIdIgnoringPause(ctx, recipient.String())
	if err != nil {
		return err
	}
	if err := recordPing(sender.UserPostgresInfo.Id, recipient); err != nil {
		return err
	}
	if len(tokens) == 0 {
		// Recorded all the same: they'll see the DM, and one ping a day holds.
		return nil
	}
	name := sender.UserDgraphInfo.DisplayName()
	data := map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:     firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:  grp,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:    "Urgent from " + name,
		firebaseInit.FIREBASE_PUSH_DATA_BODY:     name + " says it can't wait. Open your messages.",
		firebaseInit.FIREBASE_PUSH_DATA_USERNAME: name,
		firebaseInit.FIREBASE_PUSH_DATA_ICON:     userBusiness.GetSignedProfileURL(ctx, sender.UserDgraphInfo.ProfileKey),
	}
	if err := firebaseInit.FirebaseApp.MultiCastPush(ctx, data, tokens); err != nil {
		helpers.LogErrorWithContext(ctx, "notification/NotifyAnyway push err: %+v", err)
		return errors.New("couldn't reach their devices")
	}
	return nil
}
