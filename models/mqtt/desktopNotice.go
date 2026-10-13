package models

import "time"

// MqttDesktopNotice is one notification for the desktop app to raise
// (MESSAGE_DESKTOP_NOTICE). NoticeID is the same for every copy of one
// notification (a redelivery, a second window), so the app raises it once;
// SentAt lets it drop a notice the broker kept while it was away, rather than
// raise a burst of old ones. Data is the push's own data
// (initializers/firebaseInit's keys: type, type_id, thread_id, title, body,
// icon), so the app opens it as it would open a push.
type MqttDesktopNotice struct {
	NoticeID string `json:"notice_id"`
	// SentAt is UTC with exactly three digits of milliseconds
	// (DesktopNoticeTime), the one form every webview's Date.parse reads:
	// Safari's is strict about the rest.
	SentAt string            `json:"sent_at"`
	Data   map[string]string `json:"data"`
}

// DesktopNoticeTime is t as a notice's SentAt.
func DesktopNoticeTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
