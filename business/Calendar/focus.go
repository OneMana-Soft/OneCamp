package Calendar

import (
	"context"
	"errors"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/google/uuid"
)

// EndFocus ends the focus time who is in now, wherever it was started: the
// web, the phone, the desktop app, the calendar or another browser. Each
// focus event of theirs running now, by the rule that holds their
// notifications (prefModels.RunningFocusEvents), ends now through
// UpdateEvent, keeping its title, start and notes. It answers the ids it
// ended, none when no focus time runs. One it can't end is an error, after
// the rest are ended.
//
// The web's End once wrote only the event its own browser had started, kept
// in that browser's storage, and opened the calendar for any other; focus
// time started anywhere else could not be ended from the app (23 Oct 2026).
func EndFocus(ctx context.Context, who *model.UserInfo) ([]string, error) {
	ids, err := prefModels.RunningFocusEvents(ctx, personOf(who))
	if err != nil {
		return nil, err
	}
	ended := make([]string, 0, len(ids))
	var failed []error
	for _, id := range ids {
		if err := endFocusEvent(ctx, who, id); err != nil {
			helpers.LogErrorWithContext(ctx, "business/EndFocus couldn't end focus event %s err: %+v", id, err)
			failed = append(failed, err)
			continue
		}
		ended = append(ended, id.String())
	}
	return ended, errors.Join(failed...)
}

// endFocusEvent moves a running focus event's end to now, saved as the
// calendar would save it with nothing else changed.
func endFocusEvent(ctx context.Context, who *model.UserInfo, id uuid.UUID) error {
	ev, err := GetEvent(ctx, who, id.String())
	if err != nil {
		return err
	}
	focus := true
	return UpdateEvent(ctx, id, adapter.CreateOrUpdateEventInput{
		Title:       ev.Title,
		Description: ev.Description,
		StartTime:   ev.StartTime.UTC().Format(time.RFC3339Nano),
		EndTime:     time.Now().UTC().Format(time.RFC3339Nano),
		IsFocus:     &focus,
	}, who)
}
