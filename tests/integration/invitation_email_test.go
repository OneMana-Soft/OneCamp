//go:build integration
// +build integration

package integration_test

// On OneCamp Cloud a workspace's email is lent, with a daily cap shared with
// password resets (EMAIL_DAILY_CAP): an admin inviting a whole imported team
// spent the day's emails, and someone locked out then got no reset.
// Invitations now stop a few short of the cap, keeping those for password
// resets, each answer says whether its email went and why not (email_sent,
// email_error), and an import's invitation offer says how many more can be
// emailed today.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnInvitationSaysWhetherItsEmailWent -v

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	importBusiness "github.com/akashc777/OneCamp/business/Import"
	importController "github.com/akashc777/OneCamp/controllers/Import"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/akashc777/OneCamp/tests/integration"
)

type fakeResend struct {
	mu      sync.Mutex
	answers []int // one status per call, in order
	calls   int
}

func (f *fakeResend) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := http.StatusOK
	if f.calls < len(f.answers) {
		status = f.answers[f.calls]
	}
	f.calls++
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"m1"}`))}, nil
}

func TestAnInvitationSaysWhetherItsEmailWent(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESEND_API_KEY", "re_test")
	t.Setenv("EMAIL_DAILY_CAP", "4") // three invitations a day; one (a quarter) kept for password resets
	resend := &fakeResend{answers: []int{http.StatusOK, http.StatusInternalServerError}}
	oldClient := emailService.SharedHTTPClient
	emailService.SharedHTTPClient = &http.Client{Transport: resend}
	t.Cleanup(func() { emailService.SharedHTTPClient = oldClient })
	if left, capped := emailService.InvitationsLeftToday(); !capped || left != 3 {
		t.Fatalf("today's allowance is %d (capped %v); this test needs a day nothing has been sent on", left, capped)
	}

	admin, job := uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username) VALUES ($1, 'admin@onemana.dev', 'admin')`, admin); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status, completed_at, triggered_by)
		VALUES ($1, 'asana', 'Acme', 'api', 'completed', NOW(), $2)`, job, admin); err != nil {
		t.Fatal(err)
	}
	asAdmin := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: admin, EmailID: "admin@onemana.dev", IsAdmin: true},
	})
	room := func() *int {
		t.Helper()
		rc := chi.NewRouteContext()
		rc.URLParams.Add("jobId", job.String())
		rec := httptest.NewRecorder()
		importController.HandleImportPeople(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(asAdmin, chi.RouteCtxKey, rc)))
		var offer importBusiness.ImportPeople
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &offer) != nil || !offer.Email.On {
			t.Fatalf("offer: %d %s", rec.Code, rec.Body.String())
		}
		return offer.Email.Left
	}
	invite := func(email string) (bool, string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"email": email})
		rec := httptest.NewRecorder()
		userController.AddInvitation(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(asAdmin))
		var body struct {
			Sent  bool   `json:"email_sent"`
			Error string `json:"email_error"`
			Link  string `json:"invite_link"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Link == "" {
			t.Fatalf("inviting %s: %d %s", email, rec.Code, rec.Body.String())
		}
		return body.Sent, body.Error
	}

	if left := room(); left == nil || *left != 3 {
		t.Fatalf("the offer's allowance: %v, want 3", left)
	}
	if sent, why := invite("ada@outlook.com"); !sent || why != "" {
		t.Errorf("an email the provider accepted: sent %v, %q", sent, why)
	}
	if sent, why := invite("bo@outlook.com"); sent || !strings.Contains(why, "had a problem") {
		t.Errorf("an email the provider refused: sent %v, %q", sent, why)
	}
	if sent, why := invite("cy@outlook.com"); !sent || why != "" {
		t.Errorf("the third, within the allowance: sent %v, %q", sent, why)
	}
	if sent, why := invite("dee@outlook.com"); sent || !strings.Contains(why, "kept for password resets") {
		t.Errorf("an invitation past the allowance: sent %v, %q", sent, why)
	}
	if resend.calls != 3 {
		t.Errorf("Resend was asked %d times, want 3: the fourth was over the allowance", resend.calls)
	}
	if left := room(); left == nil || *left != 0 {
		t.Errorf("the offer's allowance after three: %v, want 0", left)
	}
	var invited int
	if err := env.PG.QueryRow(`SELECT count(*) FROM invitations`).Scan(&invited); err != nil || invited != 4 {
		t.Errorf("%d invitations (%v), want all 4: one past the allowance is still invited, with a link", invited, err)
	}
}
