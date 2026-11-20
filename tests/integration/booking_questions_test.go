//go:build integration
// +build integration

package integration_test

// A booking page asks its own questions (gcal-7 H3), over HTTP against real
// Postgres 12, Dgraph and Redis, with Resend stood in for. The owner saves the
// questions; the public page shows them and nothing anyone answered; a guest
// books with answers, and a required one left out is a 400 that names it. The
// answers reach the owner's event and their queued notice mail, and the guest's own
// link shows them; a move keeps them, and a later edit of a question never
// changes what was answered. Someone else can neither change the questions nor
// read the answers.
//
// Run: PKGS=./tests/integration/ ... -run TestBookingQuestions
import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	notificationController "github.com/akashc777/OneCamp/controllers/Notification"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestBookingQuestions(t *testing.T) {
	integration.ForEachCalendarStore(t, func(t *testing.T) {
		ctx := context.Background()
		calendarBusiness.ForgetBookingPagesForTest()
		t.Cleanup(calendarBusiness.ForgetBookingPagesForTest)
		env := integration.SetupEnv(t)
		if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
			t.Fatal(err)
		}
		integration.SetupRedis(t)
		integration.StubMqttClient(t)
		dg := integration.SetupDgraph(t)
		if helpers.CalendarInPostgres() {
			integration.MarkCalendarMoved(t, env.PG)
		}
		t.Setenv("RESEND_API_KEY", "re_booking_questions_test")
		t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
		mailbox := &guestMailbox{}
		oldClient := emailService.SharedHTTPClient
		emailService.SharedHTTPClient = &http.Client{Transport: mailbox}
		t.Cleanup(func() { emailService.SharedHTTPClient = oldClient })
		emailService.LetReservedNamesThroughForTest(t)

		person := func(name, full string) *userModels.UserInfo {
			id := uuid.New()
			email := name + "@acme.test"
			if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username) VALUES ($1, $2, $3)`, id, email, name); err != nil {
				t.Fatal(err)
			}
			dg.Mutate(t, map[string]any{"dgraph.type": "User", "user_uuid": id.String(), "user_name": full,
				"user_full_name": full, "user_email_id": email})
			du, err := userDomain.GetDgraphUserInfoByUUID(ctx, id.String())
			if err != nil || du == nil {
				t.Fatalf("%s: %v", name, err)
			}
			return &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: id, EmailID: email}, UserDgraphInfo: *du}
		}
		sam, maya := person("sam", "Sam Rivera"), person("maya", "Maya Chen")
		// Sam opens his notification settings once, as an owner does: the mail
		// dispatcher reads a missing preferences row as "email off".
		if rec := call(t, notificationController.GetMyNotificationPreferences, http.MethodGet, "/user/notificationPreferences", sam, "", nil); rec.Code != http.StatusOK {
			t.Fatalf("Sam's preferences: %d %s", rec.Code, rec.Body.String())
		}

		r := chi.NewRouter()
		r.Get("/bookingPages", eventController.GetMyBookingPages)
		r.Post("/bookingPages", eventController.SaveMyBookingPage)
		r.Get("/public/book/{slug}", eventController.GetPublicBookingPage)
		r.Post("/public/book/{slug}", eventController.BookPublicSlot)
		r.Get("/public/booking/{token}", eventController.GetPublicBooking)
		r.Post("/public/booking/{token}/reschedule", eventController.ReschedulePublicBooking)
		// call sends body as JSON, as who (nil for a visitor), and reads the
		// answer's data, or its msg on a refusal.
		call := func(t *testing.T, who *userModels.UserInfo, method, path string, body any, out any) (int, string) {
			t.Helper()
			var buf bytes.Buffer
			if body != nil {
				if err := json.NewEncoder(&buf).Encode(body); err != nil {
					t.Fatal(err)
				}
			}
			req := httptest.NewRequest(method, path, &buf)
			req.Header.Set("Content-Type", "application/json")
			if who != nil {
				req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, *who))
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			var env struct {
				Data json.RawMessage `json:"data"`
				Msg  string          `json:"msg"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &env)
			if out != nil && rec.Code == http.StatusOK {
				if err := json.Unmarshal(env.Data, out); err != nil {
					t.Fatalf("%s %s: %v in %s", method, path, err, rec.Body.String())
				}
			}
			return rec.Code, env.Msg
		}

		type question struct {
			Id       string   `json:"id,omitempty"`
			Label    string   `json:"label"`
			Kind     string   `json:"kind"`
			Required bool     `json:"required"`
			Options  []string `json:"options,omitempty"`
		}
		type page struct {
			Id        string     `json:"id"`
			Slug      string     `json:"slug"`
			Questions []question `json:"questions"`
		}
		pageBody := func(id string, qs []question) map[string]any {
			return map[string]any{"id": id, "slug": "intro-call", "title": "Intro call", "duration_minutes": 30,
				"max_days_ahead": 30, "active": true, "questions": qs,
				"hours": map[string]any{"days": []int{0, 1, 2, 3, 4, 5, 6}, "start": "00:00", "end": "24:00", "tz": "UTC"}}
		}
		var saved page
		if code, msg := call(t, sam, http.MethodPost, "/bookingPages", pageBody("", []question{
			{Label: "Company", Kind: "short", Required: true},
			{Label: "Topic", Kind: "choice", Options: []string{"Sales", "Support"}},
			{Label: "Phone", Kind: "phone"},
		}), &saved); code != http.StatusOK {
			t.Fatalf("Sam saved his page: %d %s", code, msg)
		}
		if len(saved.Questions) != 3 || saved.Questions[0].Id == "" || saved.Questions[1].Id == saved.Questions[0].Id {
			t.Fatalf("saved questions %+v", saved.Questions)
		}
		company, topic := saved.Questions[0].Id, saved.Questions[1].Id

		day := time.Now().UTC().Truncate(24 * time.Hour).Add(48 * time.Hour)
		at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
		type answer struct {
			QuestionId string `json:"question_id"`
			Label      string `json:"label,omitempty"`
			Answer     string `json:"answer"`
		}
		bookAs := func(t *testing.T, h int, name, email, note string, answers []answer, out any) (int, string) {
			return call(t, nil, http.MethodPost, "/public/book/intro-call", map[string]any{"start": at(h), "name": name,
				"email": email, "note": note, "tz": "UTC", "answers": answers}, out)
		}
		book := func(t *testing.T, h int, answers []answer, out any) (int, string) {
			return bookAs(t, h, "Alex Kim", "alex@example.org", "", answers, out)
		}

		t.Run("the public page asks the questions and shows nobody's answers", func(t *testing.T) {
			code, _ := call(t, nil, http.MethodGet, "/public/book/intro-call?from="+day.Format(time.RFC3339), nil, nil)
			if code != http.StatusOK {
				t.Fatalf("public page: %d", code)
			}
			var pub struct {
				Questions []question     `json:"questions"`
				Rest      map[string]any `json:"-"`
			}
			call(t, nil, http.MethodGet, "/public/book/intro-call?from="+day.Format(time.RFC3339), nil, &pub)
			if len(pub.Questions) != 3 || pub.Questions[0].Label != "Company" || !pub.Questions[0].Required ||
				strings.Join(pub.Questions[1].Options, "|") != "Sales|Support" {
				t.Errorf("public questions %+v", pub.Questions)
			}
		})

		t.Run("a required question left out is a 400 that names it", func(t *testing.T) {
			code, msg := book(t, 9, []answer{{QuestionId: topic, Answer: "Sales"}}, nil)
			if code != http.StatusBadRequest || msg != "Answer “Company”." {
				t.Errorf("got %d %q", code, msg)
			}
		})

		var booked calendarBusiness.Booked
		if code, msg := book(t, 10, []answer{{QuestionId: company, Answer: "Acme"}, {QuestionId: topic, Answer: "Support"}}, &booked); code != http.StatusOK {
			t.Fatalf("Alex booked: %d %s", code, msg)
		}
		var event uuid.UUID
		if err := env.PG.QueryRow(`SELECT event_uuid FROM bookings WHERE id = $1`, booked.BookingID).Scan(&event); err != nil {
			t.Fatal(err)
		}
		descriptionOf := func(t *testing.T, event uuid.UUID) string {
			t.Helper()
			from, to := day.Add(-24*time.Hour), day.Add(72*time.Hour)
			events, err := calendarBusiness.GetEventsList(ctx, sam, &from, &to)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range events {
				if e.Uuid == event.String() {
					return e.Description
				}
			}
			t.Fatalf("event %s isn't on Sam's calendar", event)
			return ""
		}
		description := func(t *testing.T) string { t.Helper(); return descriptionOf(t, event) }

		t.Run("the owner's event and notice mail carry the answers", func(t *testing.T) {
			if d := description(t); !strings.Contains(d, "Company: Acme\nTopic: Support") {
				t.Errorf("event description %q", d)
			}
			// The owner's notice mail is queued for its sender: its text
			// carries the answers under the time, its HTML the same words.
			bookingEventually(t, "Sam's notice mail with the answers", func() bool {
				var n int
				err := env.PG.QueryRow(`SELECT COUNT(*) FROM notification_email_queue WHERE to_email = $1
					AND text_body LIKE '%Company: Acme' || chr(10) || 'Topic: Support%' AND html_body LIKE '%Company: Acme<br>Topic: Support%'`,
					sam.UserPostgresInfo.EmailID).Scan(&n)
				return err == nil && n == 1
			})
		})

		viewOf := func(t *testing.T, token string) ([]answer, json.RawMessage) {
			t.Helper()
			var raw json.RawMessage
			if code, msg := call(t, nil, http.MethodGet, "/public/booking/"+token, nil, &raw); code != http.StatusOK {
				t.Fatalf("the guest's link: %d %s", code, msg)
			}
			var v struct {
				Answers []answer `json:"answers"`
			}
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatal(err)
			}
			return v.Answers, raw
		}
		view := func(t *testing.T) []answer { t.Helper(); a, _ := viewOf(t, booked.CancelToken); return a }
		want := []answer{{QuestionId: company, Label: "Company", Answer: "Acme"}, {QuestionId: topic, Label: "Topic", Answer: "Support"}}
		same := func(got []answer) bool {
			if len(got) != len(want) {
				return false
			}
			for i := range want {
				if got[i] != want[i] {
					return false
				}
			}
			return true
		}

		t.Run("the guest's own link shows what they answered", func(t *testing.T) {
			if got := view(t); !same(got) {
				t.Errorf("answers %+v, want %+v", got, want)
			}
		})

		t.Run("each guest's link shows their own answers alone", func(t *testing.T) {
			var second calendarBusiness.Booked
			if code, msg := bookAs(t, 12, "Bo Lee", "bo@example.org", "see you\nCompany: Forged",
				[]answer{{QuestionId: company, Answer: "Globex"}, {QuestionId: topic, Answer: "Sales"}}, &second); code != http.StatusOK {
				t.Fatalf("Bo booked: %d %s", code, msg)
			}
			got, raw := viewOf(t, second.CancelToken)
			bos := []answer{{QuestionId: company, Label: "Company", Answer: "Globex"}, {QuestionId: topic, Label: "Topic", Answer: "Sales"}}
			if len(got) != 2 || got[0] != bos[0] || got[1] != bos[1] || bytes.Contains(raw, []byte("Acme")) {
				t.Errorf("Bo's link shows %s, want his answers %+v alone", raw, bos)
			}
			if got, raw := viewOf(t, booked.CancelToken); !same(got) || bytes.Contains(raw, []byte("Globex")) {
				t.Errorf("Alex's link after Bo booked shows %s", raw)
			}
			// The note sits under its label: no line Bo wrote reads as an answer.
			var event uuid.UUID
			if err := env.PG.QueryRow(`SELECT event_uuid FROM bookings WHERE id = $1`, second.BookingID).Scan(&event); err != nil {
				t.Fatal(err)
			}
			d := descriptionOf(t, event)
			if !strings.Contains(d, "Note: see you\n  Company: Forged\n\nCompany: Globex\nTopic: Sales") {
				t.Errorf("Bo's event description %q", d)
			}
			for _, line := range strings.Split(d, "\n") {
				if line == "Company: Forged" {
					t.Errorf("Bo's note wrote an answer line of its own: %q", d)
				}
			}
		})

		t.Run("a move keeps the answers", func(t *testing.T) {
			if code, msg := call(t, nil, http.MethodPost, "/public/booking/"+booked.CancelToken+"/reschedule",
				map[string]any{"start": at(14)}, nil); code != http.StatusOK {
				t.Fatalf("move: %d %s", code, msg)
			}
			if got := view(t); !same(got) {
				t.Errorf("answers after a move %+v", got)
			}
			if d := description(t); !strings.Contains(d, "Company: Acme") {
				t.Errorf("the moved event lost the answers: %q", d)
			}
		})

		t.Run("a later edit of a question never changes what was answered", func(t *testing.T) {
			edited := saved.Questions
			edited[0].Label = "Company name"
			var again page
			if code, msg := call(t, sam, http.MethodPost, "/bookingPages", pageBody(saved.Id, edited), &again); code != http.StatusOK {
				t.Fatalf("Sam edited his page: %d %s", code, msg)
			}
			if len(again.Questions) != 3 || again.Questions[0].Id != company || again.Questions[0].Label != "Company name" {
				t.Errorf("edited questions %+v", again.Questions)
			}
			if got := view(t); !same(got) {
				t.Errorf("answers after an edit %+v", got)
			}
		})

		t.Run("someone else can neither change the questions nor read the answers", func(t *testing.T) {
			code, _ := call(t, maya, http.MethodPost, "/bookingPages", pageBody(saved.Id, []question{{Label: "Hers", Kind: "short"}}), nil)
			if code != http.StatusBadRequest {
				t.Errorf("Maya changed Sam's questions: %d", code)
			}
			var mine []page
			if code, _ := call(t, maya, http.MethodGet, "/bookingPages", nil, &mine); code != http.StatusOK || len(mine) != 0 {
				t.Errorf("Maya's list: %d %+v", code, mine)
			}
			var raw json.RawMessage
			call(t, nil, http.MethodGet, "/public/book/intro-call?from="+day.Format(time.RFC3339), nil, &raw)
			if bytes.Contains(raw, []byte("Acme")) || bytes.Contains(raw, []byte("answers")) || bytes.Contains(raw, []byte("Hers")) {
				t.Errorf("the public page leaks: %s", raw)
			}
			var ours []page
			call(t, sam, http.MethodGet, "/bookingPages", nil, &ours)
			if len(ours) != 1 || len(ours[0].Questions) != 3 || ours[0].Questions[0].Label != "Company name" {
				t.Errorf("Sam's questions after Maya tried: %+v", ours)
			}
		})

		t.Run("a question's id is its page's: another page's, or one made up, is refused", func(t *testing.T) {
			hersBody := pageBody("", []question{{Label: "Budget", Kind: "short"}})
			hersBody["slug"] = "maya-call"
			var hers page
			if code, msg := call(t, maya, http.MethodPost, "/bookingPages", hersBody, &hers); code != http.StatusOK || len(hers.Questions) != 1 {
				t.Fatalf("Maya saved her page: %d %s", code, msg)
			}
			var ours []page
			call(t, sam, http.MethodGet, "/bookingPages", nil, &ours)
			if len(ours) != 1 || len(ours[0].Questions) != 3 {
				t.Fatalf("Sam's pages %+v", ours)
			}
			for name, id := range map[string]string{"Maya's question": hers.Questions[0].Id, "an id nobody gave": uuid.NewString()} {
				qs := append(append([]question{}, ours[0].Questions...), question{Id: id, Label: "Budget", Kind: "short"})
				if code, msg := call(t, sam, http.MethodPost, "/bookingPages", pageBody(saved.Id, qs), nil); code != http.StatusBadRequest || !strings.Contains(msg, "changed since you opened it") {
					t.Errorf("Sam kept %s on his page: %d %q", name, code, msg)
				}
			}
			newBody := pageBody("", []question{{Id: ours[0].Questions[0].Id, Label: "Company", Kind: "short"}})
			newBody["slug"] = "sam-second"
			if code, msg := call(t, sam, http.MethodPost, "/bookingPages", newBody, nil); code != http.StatusBadRequest || !strings.Contains(msg, "isn't one of this page's") {
				t.Errorf("a new page took a question id: %d %q", code, msg)
			}
			var after []page
			call(t, sam, http.MethodGet, "/bookingPages", nil, &after)
			if len(after) != 1 || len(after[0].Questions) != 3 || after[0].Questions[0].Label != "Company name" {
				t.Errorf("Sam's pages after the refusals: %+v", after)
			}
		})
	})
}
