package domain

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/google/uuid"
)

// LoadOrCreate returns the user's preference row, creating one with defaults
// if it doesn't exist. Idempotent and safe under concurrent calls because
// the underlying upsert uses ON CONFLICT DO UPDATE.
func LoadOrCreate(ctx context.Context, userID uuid.UUID) (*models.UserNotificationPreference, error) {
	pref, err := models.GetByUserID(userID)
	if err == nil {
		return pref, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx,
			"domain/UserNotificationPreference/LoadOrCreate get err: %+v", err)
		return nil, err
	}

	token, err := newUnsubscribeToken()
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UserNotificationPreference/LoadOrCreate token err: %+v", err)
		return nil, err
	}
	pref, err = models.EnsureForUser(userID, token)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UserNotificationPreference/LoadOrCreate ensure err: %+v", err)
		return nil, err
	}
	return pref, nil
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
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
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

// LoadByUserIDs is the batched twin of LoadOrCreate. Unlike LoadOrCreate,
// it does NOT auto-create missing rows — the dispatcher treats a missing
// row the same as "EmailEnabled=false", so we skip the I/O of creating
// rows for users who haven't opted in. Use LoadOrCreate from explicit
// per-user surfaces (settings dialog) and LoadByUserIDs from fan-out
// paths.
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
