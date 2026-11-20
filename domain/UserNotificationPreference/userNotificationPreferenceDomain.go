package domain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	models "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/google/uuid"
)

// LoadOrCreate returns the user's preference row, making it with the
// defaults if they have none (LoadOrCreateMany, the one way a row is made).
func LoadOrCreate(ctx context.Context, userID uuid.UUID) (*models.UserNotificationPreference, error) {
	prefs, err := LoadOrCreateMany(ctx, []uuid.UUID{userID})
	if err != nil {
		return nil, err
	}
	if prefs[userID] == nil {
		return nil, errors.New("domain/UserNotificationPreference/LoadOrCreate: no row made")
	}
	return prefs[userID], nil
}

// LoadOrCreateMany is the preference rows of the given people, making the
// rows of those who have none in one statement for all of them: the columns'
// defaults (the only place a default is written: every kind on, email only
// while offline, no digest, no quiet hours) and an unsubscribe token each,
// so the first email a person is sent carries its link. The dispatcher asks
// for the people an email is about to go to, so no row is made for anyone
// else. When rows can't be made, the rows read are returned with the error.
// Safe under concurrent calls: a row made meanwhile is returned as it is.
func LoadOrCreateMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*models.UserNotificationPreference, error) {
	out, err := LoadByUserIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	var missing, tokens []string
	asked := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		if out[id] != nil || asked[id] {
			continue
		}
		asked[id] = true
		token, err := newUnsubscribeToken()
		if err != nil {
			helpers.LogErrorWithContext(ctx, "domain/UserNotificationPreference/LoadOrCreateMany token err: %+v", err)
			return out, err
		}
		missing, tokens = append(missing, id.String()), append(tokens, token)
	}
	if len(missing) == 0 {
		return out, nil
	}
	made, err := pgexec.Query(ctx, makeDefaultRows(), models.Scan, pgexec.Array(missing), pgexec.Array(tokens))
	for _, p := range made {
		out[p.UserID] = p
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UserNotificationPreference/LoadOrCreateMany err: %+v", err)
		return out, err
	}
	return out, nil
}

// makeDefaultRows makes the rows of the people in $1, each with the token at
// its place in $2 and the columns' defaults for the rest, and returns them; a
// row someone else made first is returned as it is (its token kept).
func makeDefaultRows() string {
	return `INSERT INTO users_notification_preferences (user_id, unsubscribe_token)
		SELECT a.u, a.t FROM unnest($1::uuid[], $2::text[]) AS a(u, t)
		ON CONFLICT (user_id) DO UPDATE SET unsubscribe_token = users_notification_preferences.unsubscribe_token
		RETURNING ` + models.AllColumns()
}

// Update is a passthrough to the model layer with logging.
func Update(ctx context.Context, userID uuid.UUID, in models.UpdatePreferenceInput) error {
	if err := models.Update(userID, in); err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UserNotificationPreference/Update err: %+v", err)
		return err
	}
	return nil
}

// SetEmailEnabledByToken is the public unsubscribe entry point.
func SetEmailEnabledByToken(ctx context.Context, token string, enabled bool) error {
	if err := models.SetEmailEnabledByToken(token, enabled); err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UserNotificationPreference/SetEmailEnabledByToken err: %+v", err)
		return err
	}
	return nil
}

// GetByUnsubscribeToken returns the row for a one-click unsubscribe link.
// Returns nil, nil when the token is unknown so the controller can render
// a generic "already unsubscribed" page without leaking presence info.
func GetByUnsubscribeToken(ctx context.Context, token string) (*models.UserNotificationPreference, error) {
	pref, err := models.GetByUnsubscribeToken(token)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UserNotificationPreference/GetByUnsubscribeToken err: %+v", err)
		return nil, err
	}
	return pref, nil
}

func newUnsubscribeToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// LoadByUserIDs is the rows the given people have, making none: for readers
// to whom a missing row means the defaults (the push gate, read receipts).
// What emails people asks LoadOrCreateMany.
func LoadByUserIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*models.UserNotificationPreference, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]*models.UserNotificationPreference{}, nil
	}
	out, err := models.GetByUserIDs(ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UserNotificationPreference/LoadByUserIDs err: %+v", err)
		return nil, err
	}
	return out, nil
}

// SetPause pauses a person's notifications until a time, or resumes them (nil),
// making their preference row if they have none.
// MaxPause is the longest a pause may run. Longer absences belong to the
// per-kind switches, and a forgotten pause should end on its own.
const MaxPause = 7 * 24 * time.Hour

// PauseError is a pause the person asked for that can't be honoured; its text
// is written for them.
type PauseError struct{ msg string }

func (e *PauseError) Error() string { return e.msg }

// CheckPauseEnd says whether a pause may end at until. Pure.
func CheckPauseEnd(until, now time.Time) error {
	if !until.After(now) {
		return &PauseError{"Pick a time in the future."}
	}
	if until.After(now.Add(MaxPause)) {
		return &PauseError{"Pause for up to a week."}
	}
	return nil
}

// SetPause pauses every notification to the person's devices until the given
// moment, or resumes them when until is nil. Push and email both honour it.
func SetPause(ctx context.Context, userID uuid.UUID, until *time.Time) (*models.UserNotificationPreference, error) {
	if until != nil {
		if err := CheckPauseEnd(*until, time.Now()); err != nil {
			return nil, err
		}
		u := until.UTC()
		until = &u
	}
	if _, err := LoadOrCreate(ctx, userID); err != nil {
		return nil, err
	}
	if err := models.SetPausedUntil(userID, until); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UserNotificationPreference/SetPause err: %+v", err)
		return nil, err
	}
	return models.GetByUserID(userID)
}
