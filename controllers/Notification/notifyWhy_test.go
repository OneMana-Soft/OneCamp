package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	notifyBusiness "github.com/akashc777/OneCamp/business/Notify"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

var (
	sessionID = uuid.MustParse("6a0b8f3e-1111-4c55-9d1e-2b3c4d5e6f70")
	otherID   = uuid.MustParse("6a0b8f3e-2222-4c55-9d1e-2b3c4d5e6f70")
)

// signedIn is r as the session's person.
func signedIn(r *http.Request) *http.Request {
	var info userModels.UserInfo
	info.UserPostgresInfo.Id = sessionID
	return r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, info))
}

// notifyRoutes are the two routes as the router mounts them.
func notifyRoutes() http.Handler {
	r := chi.NewRouter()
	r.Post("/notify/test", SendMeATest)
	r.Get("/notify/why/{message_id}", WhyAmISeeingThis)
	return r
}

func serve(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	notifyRoutes().ServeHTTP(rec, r)
	return rec
}

// A test is the session's own, whoever the request names: it answers the
// router's steps for them.
func TestSendMeATestIsTheSessionsOwnAndAnswersEachStep(t *testing.T) {
	var asked []string
	old := testNotice
	testNotice = func(_ context.Context, asker string, _ time.Time) (notifyBusiness.TestResult, error) {
		asked = append(asked, asker)
		return notifyBusiness.TestResult{
			Trace: notifyBusiness.Trace{UserID: asker, Steps: []notifyBusiness.Step{{Step: "hold", Verdict: "not_held"}}},
			Told:  []notifyBusiness.Delivery{notifyBusiness.ToPush},
		}, nil
	}
	t.Cleanup(func() { testNotice = old })

	body := `{"user_uuid":"` + otherID.String() + `","user_id":"` + otherID.String() + `"}`
	rec := serve(signedIn(httptest.NewRequest(http.MethodPost, "/notify/test?user_uuid="+otherID.String(), strings.NewReader(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("a test answered %d: %s", rec.Code, rec.Body)
	}
	if len(asked) != 1 || asked[0] != sessionID.String() {
		t.Fatalf("the test was for %v; want the session's person alone, never one the request names", asked)
	}
	var got notifyBusiness.TestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Steps) != 1 || got.Steps[0].Verdict != "not_held" || len(got.Told) != 1 {
		t.Errorf("the answer is %s; want the router's steps", rec.Body)
	}
}

// A second test within the minute answers 429 with Retry-After and when.
func TestASecondTestWithinTheMinuteAnswers429WithItsRetryTime(t *testing.T) {
	old := testNotice
	testNotice = func(_ context.Context, _ string, now time.Time) (notifyBusiness.TestResult, error) {
		return notifyBusiness.TestResult{}, &notifyBusiness.TooSoon{RetryAt: now.Add(20 * time.Second)}
	}
	t.Cleanup(func() { testNotice = old })

	rec := serve(signedIn(httptest.NewRequest(http.MethodPost, "/notify/test", nil)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a second test answered %d; want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "20" && ra != "21" {
		t.Errorf("Retry-After is %q; want about 20 seconds", ra)
	}
	var got struct {
		Code    string    `json:"code"`
		RetryAt time.Time `json:"retry_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Code != "too_soon" || got.RetryAt.IsZero() {
		t.Errorf("the refusal is %s; want code too_soon and retry_at", rec.Body)
	}
}

// Why answers the session's own record of the message in the path, and 404
// (never 403) for one that told them nothing, the same words either way.
func TestWhyOfAnItemThatIsntYoursIs404(t *testing.T) {
	var asked [][2]string
	old := whyOf
	whyOf = func(_ context.Context, asker, message string) ([]notifyBusiness.Trace, error) {
		asked = append(asked, [2]string{asker, message})
		if message == "mine" {
			return []notifyBusiness.Trace{{UserID: asker, EventID: "channel:c:mine"}}, nil
		}
		return nil, notifyBusiness.ErrNoRecord
	}
	t.Cleanup(func() { whyOf = old })

	rec := serve(signedIn(httptest.NewRequest(http.MethodGet, "/notify/why/mine?user_uuid="+otherID.String(), nil)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"events"`) {
		t.Fatalf("their own item answered %d: %s", rec.Code, rec.Body)
	}
	theirs := serve(signedIn(httptest.NewRequest(http.MethodGet, "/notify/why/theirs", nil)))
	if theirs.Code != http.StatusNotFound {
		t.Errorf("another person's item answered %d; want 404, never 403", theirs.Code)
	}
	none := serve(signedIn(httptest.NewRequest(http.MethodGet, "/notify/why/none", nil)))
	if none.Code != http.StatusNotFound || none.Body.String() != theirs.Body.String() {
		t.Errorf("no record answered %d %s; want the same 404 as another person's (%s)", none.Code, none.Body, theirs.Body)
	}
	for _, a := range asked {
		if a[0] != sessionID.String() {
			t.Errorf("Why was asked for %s; want the session's person", a[0])
		}
	}
	if len(asked) != 3 || asked[0][1] != "mine" {
		t.Errorf("Why was asked %v", asked)
	}
}

// Neither route answers without a session.
func TestTheNotifyRoutesNeedASession(t *testing.T) {
	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/notify/test", nil),
		httptest.NewRequest(http.MethodGet, "/notify/why/x", nil),
	} {
		if rec := serve(r); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no session answered %d; want 401", r.Method, r.URL.Path, rec.Code)
		}
	}
}
