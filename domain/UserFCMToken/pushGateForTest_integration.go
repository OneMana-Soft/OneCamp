//go:build integration

package domain

// NotPausedAtForTest is the push gate's hold (notPaused) at the instant the
// SQL expression now names instead of NOW(), for userCol: the integration
// test asks it at each instant of a grid and holds it to the Go hold's answer
// there (models/postgres/UserNotificationPreference QuietHoursUntil). It
// exists only in the integration build, so no product code can reach it.
func NotPausedAtForTest(userCol, now string) string {
	return notPausedAt(userCol, now)
}
