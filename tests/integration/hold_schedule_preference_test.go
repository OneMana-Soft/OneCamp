//go:build integration
// +build integration

package integration_test

// A notification schedule (notify.schedule; slack-05 weekday schedules,
// basecamp-19 quiet hours from working hours) against Postgres 12, over the
// preferences routes a person sets their own through: one that follows their
// working hours is copied from them when set, and again whenever their
// working hours or zone change, and cleared once there are none to follow;
// one of their own is left alone; a refusal is in words; null clears it; and
// a teammate is told only until when they're held, never the hours.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheScheduleFollowsWorkingHoursOverHTTP -v

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	notificationController "github.com/akashc777/OneCamp/controllers/Notification"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheScheduleFollowsWorkingHoursOverHTTP(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	member := func(name string) *userModels.UserInfo {
		id := uuid.New()
		handle := strings.ToLower(name) + id.String()[:4]
		email := handle + "@example.test"
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, email, handle, name); err != nil {
			t.Fatal(err)
		}
		return &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: id, EmailID: email, Username: &handle}}
	}
	sam, maya := member("Sam"), member("Maya")
	schedule := func(who *userModels.UserInfo) string { return string(prefs(t, who)[prefModels.ScheduleKey]) }
	// same is whether a value read back is want, as JSON values.
	same := func(got, want string) bool {
		var a, b any
		return json.Unmarshal([]byte(got), &a) == nil && json.Unmarshal([]byte(want), &b) == nil && reflect.DeepEqual(a, b)
	}
	set := func(who *userModels.UserInfo, key, value string) {
		t.Helper()
		if code, body := setPref(t, who, key, value); code != http.StatusOK {
			t.Fatalf("%s = %s: %d %s", key, value, code, body)
		}
	}

	if got := schedule(sam); got != "null" {
		t.Fatalf("a schedule before any was set: %s", got)
	}
	// Following hours Sam hasn't set is refused in words, and so is a week with none.
	for value, says := range map[string]string{
		`{"working_hours":true}`:                                               "Set your working hours first.",
		`{"tz":"Asia/Kolkata","week":{}}`:                                      "Use Pause for a whole stretch off.",
		`{"tz":"Mars/Olympus","week":{"1":[]}}`:                                "Use Pause for a whole stretch off.",
		`{"tz":"Mars/Olympus","week":{"1":[{"start":"09:00","end":"17:00"}]}}`: "That isn't a time zone this server knows.",
	} {
		if code, body := setPref(t, sam, prefModels.ScheduleKey, value); code != http.StatusBadRequest || !strings.Contains(body, says) {
			t.Errorf("%s: %d %s, want %q", value, code, body, says)
		}
	}

	set(sam, "zone", `"Asia/Kolkata"`)
	set(sam, "calendar.working_hours", `{"1":[{"start":"09:00","end":"17:00"}]}`)
	set(sam, prefModels.ScheduleKey, `{"working_hours":true}`)
	want := `{"tz":"Asia/Kolkata","week":{"1":[{"start":"09:00","end":"17:00"}]},"working_hours":true}`
	if got := schedule(sam); !same(got, want) {
		t.Fatalf("copied when set: %s, want %s", got, want)
	}

	// The hook: Sam's working hours change, and so does the schedule that follows them.
	set(sam, "calendar.working_hours", `{"2":[{"start":"10:00","end":"18:00"}],"4":[{"start":"10:00","end":"18:00"}]}`)
	want = `{"tz":"Asia/Kolkata","week":{"2":[{"start":"10:00","end":"18:00"}],"4":[{"start":"10:00","end":"18:00"}]},"working_hours":true}`
	if got := schedule(sam); !same(got, want) {
		t.Errorf("after the working hours changed: %s, want %s", got, want)
	}
	// And the zone they're read in.
	set(sam, "zone", `"America/New_York"`)
	want = strings.Replace(want, "Asia/Kolkata", "America/New_York", 1)
	if got := schedule(sam); !same(got, want) {
		t.Errorf("after the zone changed: %s, want %s", got, want)
	}
	// What the hold reads for someone with no preferences row is what was copied.
	if s, err := prefModels.ScheduleOf(context.Background(), sam.UserPostgresInfo.Id); err != nil || s == nil || !s.WorkingHours || s.TZ != "America/New_York" {
		t.Errorf("ScheduleOf: %+v %v", s, err)
	}

	// Maya keeps a schedule of her own, on a day three after today (so she
	// is held now, whenever this runs): her working hours changing leaves it alone.
	day := strconv.Itoa(int((time.Now().UTC().Weekday() + 3) % 7))
	own := `{"tz":"UTC","week":{"` + day + `":[{"start":"09:00","end":"17:00"}]}}`
	set(maya, prefModels.ScheduleKey, own)
	set(maya, "zone", `"Europe/London"`)
	set(maya, "calendar.working_hours", `{"1":[{"start":"08:00","end":"16:00"}]}`)
	if got := schedule(maya); !same(got, own) {
		t.Errorf("Maya's own schedule after her working hours changed: %s, want %s", got, own)
	}
	if got := schedule(sam); strings.Contains(got, "08:00") {
		t.Errorf("Maya's working hours reached Sam's schedule: %s", got)
	}

	// What a teammate sees of Maya, held now: until when, never her hours.
	if _, err := env.PG.Exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, $2)`, maya.UserPostgresInfo.Id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if p, err := prefModels.GetByUserID(maya.UserPostgresInfo.Id); err != nil || p.Schedule == nil {
		t.Fatalf("the row read without the schedule: %+v %v", p, err)
	} else if until, held := p.QuietHoursUntil(time.Now()); !held || !until.After(time.Now()) {
		t.Errorf("Maya now: held %v until %v", held, until)
	}
	rec := call(t, notificationController.TeammateNotificationStatus, http.MethodGet, "/user/notificationStatus/"+maya.UserPostgresInfo.Id.String(), sam, "", map[string]string{"user_uuid": maya.UserPostgresInfo.Id.String()})
	var status struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &status) != nil {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	if string(status.Data["reason"]) != `"quiet_hours"` || status.Data["until"] == nil {
		t.Errorf("a teammate isn't told Maya is held: %s", rec.Body.String())
	}
	for k := range status.Data {
		if k != "until" && k != "reason" {
			t.Errorf("a teammate is told %s: %s", k, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), "17:00") || strings.Contains(rec.Body.String(), "week") {
		t.Errorf("a teammate is told the hours: %s", rec.Body.String())
	}

	// Sam's working hours cleared: nothing to follow, so the schedule goes too.
	set(sam, "calendar.working_hours", `null`)
	if got := schedule(sam); got != "null" {
		t.Errorf("after the working hours were cleared: %s", got)
	}
	// null clears a schedule of one's own.
	set(maya, prefModels.ScheduleKey, `null`)
	if got := schedule(maya); got != "null" {
		t.Errorf("after null: %s", got)
	}
	var rows int
	if err := env.PG.QueryRow(`SELECT count(*) FROM person_preferences WHERE key = $1`, prefModels.ScheduleKey).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("schedules left: %d %v", rows, err)
	}
}

// V-532 (V-505): a schedule that follows working hours is copied in the same
// change as the hours or zone it follows. When the copy can't be written,
// nothing is kept: the hours, the zone and the copy stay as they were, their
// other devices are told nothing, and the person is told in words to try
// again (the shared write-failure path), never a save that leaves their
// pushes held by hours they no longer keep.
func TestAFollowThatFailsKeepsNothingAndSaysSo(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	broker := integration.RecordMqtt(t)
	id := uuid.New()
	handle := "sam" + id.String()[:4]
	email := handle + "@example.test"
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, $2, $3, 'Sam', NOW(), NOW())`, id, email, handle); err != nil {
		t.Fatal(err)
	}
	sam := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: id, EmailID: email, Username: &handle}}
	set := func(key, value string) {
		t.Helper()
		if code, body := setPref(t, sam, key, value); code != http.StatusOK {
			t.Fatalf("%s = %s: %d %s", key, value, code, body)
		}
	}
	const nine = `{"1":[{"start":"09:00","end":"17:00"}]}`
	const ten = `{"1":[{"start":"10:00","end":"18:00"}]}`
	set("zone", `"Asia/Kolkata"`)
	set("calendar.working_hours", nine)
	set(prefModels.ScheduleKey, `{"working_hours":true}`)
	before := prefs(t, sam)
	// told is the preference keys Sam's devices were told of so far, in order.
	told := func() []string {
		var keys []string
		for _, d := range broker.Messages(helpers.GetMqttTopicForUserActivity(id.String()), mqttStruct.MESSAGE_PREFERENCE) {
			var m struct {
				Key string `json:"key"`
			}
			_ = json.Unmarshal(d, &m)
			keys = append(keys, m.Key)
		}
		return keys
	}
	toldBefore := len(told())

	// The copy can't be written: Postgres refuses any write of Sam's schedule.
	refuse := "v532_refuse_" + strings.ReplaceAll(id.String(), "-", "")
	t.Cleanup(func() {
		_, _ = env.PG.Exec(`DROP TRIGGER IF EXISTS ` + refuse + ` ON person_preferences`)
		_, _ = env.PG.Exec(`DROP FUNCTION IF EXISTS ` + refuse + `()`)
	})
	for _, q := range []string{
		`CREATE FUNCTION ` + refuse + `() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF (CASE WHEN TG_OP = 'DELETE' THEN OLD.user_id ELSE NEW.user_id END) = '` + id.String() + `'::uuid
				AND (CASE WHEN TG_OP = 'DELETE' THEN OLD.key ELSE NEW.key END) = '` + prefModels.ScheduleKey + `' THEN
				RAISE EXCEPTION 'test: the schedule can''t be written';
			END IF;
			IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
			RETURN NEW;
		END $$`,
		`CREATE TRIGGER ` + refuse + ` BEFORE INSERT OR UPDATE OR DELETE ON person_preferences FOR EACH ROW EXECUTE PROCEDURE ` + refuse + `()`,
	} {
		if _, err := env.PG.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	// New hours, a new zone, the hours cleared (the copy would go): each is
	// refused in the shared words, and nothing changed.
	for _, c := range []struct{ key, value string }{
		{"calendar.working_hours", ten},
		{"zone", `"Europe/London"`},
		{"calendar.working_hours", `null`},
	} {
		code, body := setPref(t, sam, c.key, c.value)
		if code != http.StatusServiceUnavailable || !strings.Contains(body, "Couldn't save that. Try again in a moment.") {
			t.Errorf("%s = %s while the schedule can't follow: %d %s, want 503 and words to try again", c.key, c.value, code, body)
		}
		if got := told(); len(got) != toldBefore {
			t.Errorf("after %s = %s was refused, Sam's devices were told of %v", c.key, c.value, got[toldBefore:])
		}
		after := prefs(t, sam)
		for _, key := range []string{"calendar.working_hours", "zone", prefModels.ScheduleKey} {
			if string(after[key]) != string(before[key]) {
				t.Errorf("after %s = %s was refused, %s reads %s, want %s as before", c.key, c.value, key, after[key], before[key])
			}
		}
	}

	// Once the copy can be written again, the same change goes through and
	// the schedule follows it.
	if _, err := env.PG.Exec(`DROP TRIGGER ` + refuse + ` ON person_preferences`); err != nil {
		t.Fatal(err)
	}
	set("calendar.working_hours", ten)
	if got := told()[toldBefore:]; !reflect.DeepEqual(got, []string{"calendar.working_hours", prefModels.ScheduleKey}) {
		t.Errorf("once kept, Sam's devices were told of %v, want the hours and the schedule once each", got)
	}
	want := `{"tz":"Asia/Kolkata","week":{"1":[{"start":"10:00","end":"18:00"}]},"working_hours":true}`
	var a, b any
	if got := prefs(t, sam)[prefModels.ScheduleKey]; json.Unmarshal(got, &a) != nil || json.Unmarshal([]byte(want), &b) != nil || !reflect.DeepEqual(a, b) {
		t.Errorf("after the write could be made: %s, want %s", got, want)
	}
	set(prefModels.ScheduleKey, `null`)
}
