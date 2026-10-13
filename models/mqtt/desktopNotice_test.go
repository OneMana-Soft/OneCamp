package models

import (
	"encoding/json"
	"testing"
	"time"
)

// The desktop app reads its notices by this number (onecamp-fe
// services/mqttService.ts Desktop_Notice, pinned by mqttService.enum.test.ts).
// Kept apart from TestMessageTypesKeepTheirNumbers so the two branches that
// add 38 and 39 never edit the same lines.
func TestDesktopNoticeIs39(t *testing.T) {
	if MESSAGE_DESKTOP_NOTICE != 39 {
		t.Fatalf("DESKTOP_NOTICE=%d, want 39; append new types at the end", MESSAGE_DESKTOP_NOTICE)
	}
}

// The app reads notice_id, sent_at and data by these names, and sent_at with
// exactly three digits of milliseconds whatever the clock gave.
func TestDesktopNoticeWireForm(t *testing.T) {
	cases := map[time.Time]string{
		time.Date(2026, 10, 13, 9, 0, 0, 123_456_789, time.UTC):                       "2026-10-13T09:00:00.123Z",
		time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC):                                 "2026-10-13T09:00:00.000Z",
		time.Date(2026, 10, 13, 14, 30, 0, 120_000_000, time.FixedZone("IST", 19800)): "2026-10-13T09:00:00.120Z",
	}
	for at, want := range cases {
		if got := DesktopNoticeTime(at); got != want {
			t.Errorf("DesktopNoticeTime(%v) = %s, want %s", at, got, want)
		}
	}

	b, err := json.Marshal(Message{Type: MESSAGE_DESKTOP_NOTICE, Data: &MqttDesktopNotice{
		NoticeID: "chat:g:m",
		SentAt:   DesktopNoticeTime(time.Date(2026, 10, 13, 9, 0, 0, 123_000_000, time.UTC)),
		Data:     map[string]string{"type": "chat", "title": "Dm - Maya"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":39,"data":{"notice_id":"chat:g:m","sent_at":"2026-10-13T09:00:00.123Z","data":{"title":"Dm - Maya","type":"chat"}}}`
	if got := string(b); got != want {
		t.Fatalf("wire form:\n got %s\nwant %s", got, want)
	}
}
