package email

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// A booking's confirmation carries its calendar invite (CAL-BUG-15): an
// attachment reaches Resend as base64, with its name and type.
func TestAnAttachmentReachesResend(t *testing.T) {
	oldClient, oldKey, oldMX := SharedHTTPClient, apiKeyFrom, lookupMX
	t.Cleanup(func() { SharedHTTPClient, apiKeyFrom, lookupMX = oldClient, oldKey, oldMX })
	apiKeyFrom = func() string { return "re_test" }
	lookupMX = func(context.Context, string) ([]*net.MX, error) {
		return []*net.MX{{Host: "mx.guests-of-acme.dev.", Pref: 10}}, nil
	}
	var sent map[string]any
	SharedHTTPClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &sent); err != nil {
			t.Errorf("payload %s: %v", raw, err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id":"m1"}`))}, nil
	})}

	invite := "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nEND:VCALENDAR\r\n"
	if _, err := SendEmailWithOptions(context.Background(), SendOptions{
		From: "noreply@acme.dev", To: "alex@guests-of-acme.dev", Subject: "Booked", HTML: "<p>Booked</p>",
		Attachments: []Attachment{{Filename: "invite.ics", ContentType: "text/calendar; charset=utf-8; method=REQUEST", Content: []byte(invite)}},
	}); err != nil {
		t.Fatal(err)
	}
	list, _ := sent["attachments"].([]any)
	if len(list) != 1 {
		t.Fatalf("attachments %v", sent["attachments"])
	}
	a := list[0].(map[string]any)
	content, _ := base64.StdEncoding.DecodeString(a["content"].(string))
	if a["filename"] != "invite.ics" || a["content_type"] != "text/calendar; charset=utf-8; method=REQUEST" || string(content) != invite {
		t.Errorf("attachment %v (content %q)", a, content)
	}

	// A message without one sends no attachments field at all.
	sent = nil
	if _, err := SendEmailWithOptions(context.Background(), SendOptions{
		From: "noreply@acme.dev", To: "alex@guests-of-acme.dev", Subject: "Hi", HTML: "<p>Hi</p>",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := sent["attachments"]; ok {
		t.Errorf("an empty attachments field: %v", sent)
	}
}
