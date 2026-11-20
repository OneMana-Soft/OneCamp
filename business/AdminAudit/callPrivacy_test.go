package business

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A call-privacy row is a person's own, in the calls category: a member's
// names them from their session; a guest's names no member, and the guest
// the server proved is in its metadata.
func TestCallPrivacyEntry(t *testing.T) {
	guest := httptest.NewRequest("POST", "/guest/calls/consent", nil)
	guest.RemoteAddr = "203.0.113.7:5000"
	e := callPrivacyEntry(guest, "consent_left", "Kai Lin (guest) left", map[string]interface{}{"identity": "guest-1", "guest": true})
	if e.Action != "call.privacy.consent_left" || e.Category != CategoryCalls || e.ActorKind != ActorHuman {
		t.Errorf("the row: %s %s %s", e.Action, e.Category, e.ActorKind)
	}
	if e.ActorID != nil || e.ActorEmail != "" || e.Metadata == nil || !strings.Contains(*e.Metadata, `"identity":"guest-1"`) {
		t.Errorf("a guest's row names a member, or not the guest: %v %q %v", e.ActorID, e.ActorEmail, e.Metadata)
	}
	if e.IPAddress == "" {
		t.Errorf("no address on the row")
	}

	id := uuid.New()
	var info userModels.UserInfo
	info.UserPostgresInfo.Id, info.UserPostgresInfo.EmailID = id, "maya@example.com"
	member := httptest.NewRequest("POST", "/calls/s-1/consent", nil).WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, info))
	m := callPrivacyEntry(member, "consent_accepted", strings.Repeat("x", 2000), nil)
	if m.ActorID == nil || *m.ActorID != id || m.ActorEmail != "maya@example.com" || len(m.Summary) != 1024 {
		t.Errorf("a member's row: %v %q summary %d", m.ActorID, m.ActorEmail, len(m.Summary))
	}
}
