//go:build integration
// +build integration

package integration_test

// When an invitation is accepted, every admin's app hears of it (MQTT type
// 44, on the admin broadcast topic): the members list and the invitations
// list showed "Invited" until someone reloaded. Only the joining that uses
// the invitation says so: one already used says nothing more.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnAcceptedInvitationTellsTheAdmins -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnAcceptedInvitationTellsTheAdmins(t *testing.T) {
	lr := setupLandReady(t)
	ctx := lr.ctx
	broker := &noticeBroker{}
	integration.UseMqttClient(t, broker)
	asOwner := context.WithValue(ctx, helpers.UserInfoContextKey, lr.owner)
	raw, _ := json.Marshal(map[string]string{"email": "gia@example.test"})
	rec := httptest.NewRecorder()
	userController.AddInvitation(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(asOwner))
	if rec.Code != http.StatusOK {
		t.Fatalf("inviting gia: %d %s", rec.Code, rec.Body)
	}
	accepted := func() []string {
		broker.mu.Lock()
		defer broker.mu.Unlock()
		var emails []string
		for _, m := range broker.got {
			if m.topic != helpers.GetMqttTopicForAdminBroadcast() {
				continue
			}
			var msg struct {
				Type int8                              `json:"type"`
				Data mqttStruct.MqttInvitationAccepted `json:"data"`
			}
			if json.Unmarshal(m.payload, &msg) == nil && msg.Type == mqttStruct.MESSAGE_INVITATION_ACCEPTED {
				emails = append(emails, msg.Data.Email)
			}
		}
		return emails
	}
	if _, err := userBusiness.AdmitOAuthUser(ctx, "gia@example.test", "Gia", "google", ""); err != nil {
		t.Fatalf("joining: %v", err)
	}
	waitLong(t, "the admins hear of it", 10*time.Second, func() bool { return len(accepted()) == 1 })
	if got := accepted(); got[0] != "gia@example.test" {
		t.Errorf("accepted: %v", got)
	}
	if _, err := userBusiness.AdmitOAuthUser(ctx, "gia@example.test", "Gia", "google", ""); err != nil {
		t.Fatalf("signing in again: %v", err)
	}
	if got := accepted(); len(got) != 1 {
		t.Errorf("a used invitation was announced again: %v", got)
	}
}
