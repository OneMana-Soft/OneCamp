package helpers

import "context"

// ApiAppIDContextKey carries the installed app a request's API token belongs
// to, when it is an app's credential (business/App; migration 360): set by
// the token middleware beside the token mark, so ActsThroughToken still holds
// and the app can only be narrowed, never widened. Absent for a person's or an
// agent's token, and for cookie or session requests.
const ApiAppIDContextKey ContextKey = "apiAppId"

// GetApiAppID is the app a request's token belongs to, or "".
func GetApiAppID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(ApiAppIDContextKey).(string)
	return id
}
