package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A member an admin lets invite people makes at most memberInvitesPerDay
// invitations a day; the next is refused in plain words. Admins aren't
// counted: they invite whole teams at once. (No Redis here: the count is
// kept in this process, as it is when Redis can't be reached.)
func TestAMemberInvitesAtMostFiftyADay(t *testing.T) {
	member := models.UserInfo{UserPostgresInfo: models.User{Id: uuid.New()}}
	admin := models.UserInfo{UserPostgresInfo: models.User{Id: uuid.New(), IsAdmin: true}}
	for i := 0; i < memberInvitesPerDay; i++ {
		if refuseOverInviteLimit(httptest.NewRecorder(), context.Background(), member) {
			t.Fatalf("invitation %d refused", i+1)
		}
		if refuseOverInviteLimit(httptest.NewRecorder(), context.Background(), admin) {
			t.Fatalf("an admin's invitation %d refused", i+1)
		}
	}
	rec := httptest.NewRecorder()
	if !refuseOverInviteLimit(rec, context.Background(), member) {
		t.Fatal("invitation 51 allowed")
	}
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "You can invite more tomorrow") {
		t.Errorf("the refusal: %d %s", rec.Code, rec.Body.String())
	}
	if refuseOverInviteLimit(httptest.NewRecorder(), context.Background(), admin) {
		t.Error("an admin's invitation 51 refused")
	}
}
