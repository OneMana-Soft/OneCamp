//go:build integration
// +build integration

package integration_test

// "Why did or didn't I get this?" (A4 H3) over HTTP, on a migrated
// Postgres: "Send me a test" goes through the router to the one asking
// alone, writes their rows only, and a second within the minute is refused;
// "Why am I seeing this?" answers the asker's own record of a message, and
// 404 for anyone else's, the same as none.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	notifyBusiness "github.com/akashc777/OneCamp/business/Notify"
	notificationController "github.com/akashc777/OneCamp/controllers/Notification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	deliveryModels "github.com/akashc777/OneCamp/models/postgres/NotificationDelivery"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// notifyWorld is a migrated Postgres and the two routes as the router mounts
// them, each request as the person it is sent as.
func notifyWorld(t *testing.T) (*integration.Env, func(id, method, path string) *httptest.ResponseRecorder) {
	t.Helper()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	t.Cleanup(func() { helpers.SettleBackground(10 * time.Second) })
	r := chi.NewRouter()
	r.Post("/notify/test", notificationController.SendMeATest)
	r.Get("/notify/why/{message_id}", notificationController.WhyAmISeeingThis)
	return env, func(id, method, path string) *httptest.ResponseRecorder {
		var info userModels.UserInfo
		info.UserPostgresInfo.Id = uuid.MustParse(id)
		req := httptest.NewRequest(method, path, nil)
		req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, info))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
}

// deliveryRows is every row, as "user delivery cause state".
func deliveryRows(t *testing.T, env *integration.Env) []string {
	t.Helper()
	rows, err := env.PG.Query(`SELECT user_id::text, delivery, cause, state FROM notification_deliveries ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u, d, c, s string
		if err := rows.Scan(&u, &d, &c, &s); err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.Join([]string{u, d, c, s}, " "))
	}
	return out
}

func TestSendMeATestOverHTTP(t *testing.T) {
	env, as := notifyWorld(t)
	me, other := seedUser(t, env.PG), seedUser(t, env.PG)
	// The limit is the minute's: start well inside one.
	if s := time.Now().Second(); s > 45 {
		time.Sleep(time.Duration(61-s) * time.Second)
	}

	rec := as(me, http.MethodPost, "/notify/test")
	if rec.Code != http.StatusOK {
		t.Fatalf("a test answered %d: %s", rec.Code, rec.Body)
	}
	var got notifyBusiness.TestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, s := range got.Steps {
		steps = append(steps, s.Step+" "+s.Verdict)
	}
	if want := []string{"audience site", "hold not_held"}; !slices.Equal(steps, want) {
		t.Errorf("the steps are %v; want %v", steps, want)
	}
	if !slices.Equal(got.Told, []notifyBusiness.Delivery{notifyBusiness.ToPush, notifyBusiness.ToDesktop}) {
		t.Errorf("the test told %v; want push and desktop", got.Told)
	}
	if !got.Devices.PushKnown || got.Devices.Push != 0 {
		t.Errorf("devices are %+v; want none with push, read", got.Devices)
	}
	if strings.Contains(rec.Body.String(), `"body"`) || strings.Contains(rec.Body.String(), "Test from OneCamp") {
		t.Errorf("the answer carries the notice's words: %s", rec.Body)
	}
	want := []string{me + " desktop notify.test sent", me + " push notify.test sent"}
	if rows := deliveryRows(t, env); !slices.Equal(rows, want) {
		t.Fatalf("the rows are %v; want %v", rows, want)
	}

	again := as(me, http.MethodPost, "/notify/test")
	if again.Code != http.StatusTooManyRequests {
		t.Fatalf("a second test in the minute answered %d: %s", again.Code, again.Body)
	}
	if ra, err := strconv.Atoi(again.Header().Get("Retry-After")); err != nil || ra < 1 || ra > 60 {
		t.Errorf("Retry-After is %q; want the seconds to the next minute", again.Header().Get("Retry-After"))
	}
	if !strings.Contains(again.Body.String(), `"too_soon"`) || !strings.Contains(again.Body.String(), `"retry_at"`) {
		t.Errorf("the refusal is %s; want too_soon and retry_at", again.Body)
	}
	if rows := deliveryRows(t, env); !slices.Equal(rows, want) {
		t.Errorf("the refused test wrote rows: %v", rows)
	}

	// Another person's test in the same minute is their own.
	if theirs := as(other, http.MethodPost, "/notify/test"); theirs.Code != http.StatusOK {
		t.Errorf("another person's test answered %d", theirs.Code)
	}
	for _, row := range deliveryRows(t, env) {
		if !strings.HasPrefix(row, me+" ") && !strings.HasPrefix(row, other+" ") {
			t.Errorf("a row of someone who sent no test: %s", row)
		}
	}
}

func TestWhyAmISeeingThisOverHTTP(t *testing.T) {
	env, as := notifyWorld(t)
	me, other := seedUser(t, env.PG), seedUser(t, env.PG)
	msg, theirsOnly := uuid.NewString(), uuid.NewString()
	nine := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	sent := time.Now().UTC()
	ev := "channel:" + uuid.NewString() + ":" + msg
	if _, err := deliveryModels.Record(context.Background(), []deliveryModels.Row{
		{EventID: ev, UserID: me, Delivery: "inbox", Cause: "chat.mention", State: "sent", MessageID: msg, SentAt: &sent},
		{EventID: ev, UserID: me, Delivery: "push", Cause: "chat.mention", State: "held", MessageID: msg, HoldUntil: &nine},
		{EventID: ev, UserID: other, Delivery: "push", Cause: "chat.all_posts", State: "sent", MessageID: msg, SentAt: &sent},
		{EventID: "chat:g:" + theirsOnly, UserID: other, Delivery: "push", Cause: "chat.dm", State: "sent", MessageID: theirsOnly, SentAt: &sent},
	}); err != nil {
		t.Fatal(err)
	}

	rec := as(me, http.MethodGet, "/notify/why/"+msg)
	if rec.Code != http.StatusOK {
		t.Fatalf("their own item answered %d: %s", rec.Code, rec.Body)
	}
	var got struct {
		Events []notifyBusiness.Trace `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 1 || got.Events[0].EventID != ev || got.Events[0].UserID != me {
		t.Fatalf("the answer is %s; want the one event, theirs", rec.Body)
	}
	tr := got.Events[0]
	d := tr.Decision
	if d == nil || d.Cause != notifyBusiness.CauseMention ||
		!slices.Equal(d.To, []notifyBusiness.Delivery{notifyBusiness.ToInbox}) ||
		!slices.Equal(d.Held, []notifyBusiness.Delivery{notifyBusiness.ToPush}) || d.HoldTill == nil || !d.HoldTill.Equal(nine) {
		t.Errorf("the decision is %+v; want the inbox now and the push held until %s", d, nine)
	}
	if len(tr.Deliveries) != 2 || tr.Steps[len(tr.Steps)-1].Verdict != "held" {
		t.Errorf("deliveries %+v, steps %+v; want their two, held", tr.Deliveries, tr.Steps)
	}
	if strings.Contains(rec.Body.String(), other) || strings.Contains(rec.Body.String(), "all_posts") {
		t.Errorf("the answer names another person's decision: %s", rec.Body)
	}

	notTheirs := as(me, http.MethodGet, "/notify/why/"+theirsOnly)
	if notTheirs.Code != http.StatusNotFound {
		t.Errorf("another person's item answered %d; want 404", notTheirs.Code)
	}
	for _, path := range []string{"/notify/why/" + uuid.NewString(), "/notify/why/not-a-message"} {
		if none := as(me, http.MethodGet, path); none.Code != http.StatusNotFound || none.Body.String() != notTheirs.Body.String() {
			t.Errorf("%s answered %d %s; want the same 404 as another person's item", path, none.Code, none.Body)
		}
	}
	// The same message asked by the other is their own decision alone.
	theirs := as(other, http.MethodGet, "/notify/why/"+msg)
	if theirs.Code != http.StatusOK || strings.Contains(theirs.Body.String(), me) || !strings.Contains(theirs.Body.String(), "all_posts") {
		t.Errorf("the other's record of the message is %d %s; want theirs alone", theirs.Code, theirs.Body)
	}
}
