//go:build integration
// +build integration

package integration_test

// The calendar's pushes, against real Postgres, Dgraph and Redis, with the
// events in either store.
//
// The calendar pushes in two places: a guest taken off an event
// (tellRemovedGuests), and a booking page's owner when a guest books or
// cancels (TellBookingOwner). Both go through Tell, then Deliver, then
// ReachableNow, and so through the one push gate (domain/UserFCMToken
// pushGate: stillActive AND notPaused). There is no second rule beside it.
// The gate reads focus time from whichever store holds the events. So
// someone paused, someone in their own focus time and someone deactivated
// are pushed nothing, and get no desktop notice either. The first two are
// still told in their inbox: the gate holds only the push.
//
// Run: go test -tags=integration ./tests/integration/ -run TestNoCalendarPushReachesSomeoneHeldOrDeactivated -v

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	calendarAdapter "github.com/akashc777/OneCamp/adapter/Calendar"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestNoCalendarPushReachesSomeoneHeldOrDeactivated(t *testing.T) {
	integration.ForEachCalendarStore(t, func(t *testing.T) {
		ctx := context.Background()
		env, dg := calendarStores(t)
		integration.SetupRedis(t)
		broker := &noticeBroker{}
		integration.UseMqttClient(t, broker)
		t.Setenv("RESEND_API_KEY", "")

		var mu sync.Mutex
		pushed := map[string][]string{} // a calendar push's title: the devices it went to
		t.Cleanup(firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
			if data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] != firebaseInit.FIREBASE_PUSH_DATA_TYPE_CALENDAR {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			title := data[firebaseInit.FIREBASE_PUSH_DATA_TITLE]
			pushed[title] = append(pushed[title], tokens...)
		}))
		devicesPushed := func(title string) []string {
			mu.Lock()
			defer mu.Unlock()
			devices := slices.Clone(pushed[title])
			slices.Sort(devices)
			return devices
		}
		exec := func(q string, args ...any) {
			t.Helper()
			if _, err := env.PG.Exec(q, args...); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}

		// Everyone has a phone that registered for push.
		people := map[string]userModels.UserInfo{}
		for _, name := range []string{"Sam", "Maya", "Bo", "Paul", "Fay", "Dex"} {
			p := seedCalendarPerson(t, env.PG, dg, name)
			exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', $2)`, p.UserPostgresInfo.Id, "tok-"+strings.ToLower(name))
			exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, $2)`, p.UserPostgresInfo.Id, uuid.NewString())
			people[name] = *p
		}
		id := func(name string) uuid.UUID { return people[name].UserPostgresInfo.Id }
		guest := func(name string) string { return people[name].UserDgraphInfo.Uuid }
		sam := people["Sam"]

		start := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Hour)
		in := calendarAdapter.CreateOrUpdateEventInput{Title: "Acquisition talks", StartTime: rfc(start), EndTime: rfc(start.Add(time.Hour)),
			Participants: []string{guest("Maya"), guest("Bo"), guest("Paul"), guest("Fay"), guest("Dex")}}
		out, err := calendarBusiness.CreateEvent(ctx, &sam, in)
		if err != nil {
			t.Fatal(err)
		}

		// Paul pauses his notifications, Fay is in focus time she made, and
		// Dex is deactivated. Each keeps their phone's token and their place
		// on the event.
		exec(`UPDATE users_notification_preferences SET notifications_paused_until = NOW() + INTERVAL '1 hour' WHERE user_id = $1`, id("Paul"))
		heldEvent(t, env.PG, id("Fay"), "Deep work", time.Now().Add(-10*time.Minute), time.Now().Add(50*time.Minute), true)
		if err := userBusiness.DeactivateUser(ctx, id("Dex")); err != nil {
			t.Fatalf("deactivate Dex: %v", err)
		}

		t.Run("a guest taken off an event", func(t *testing.T) {
			in.Participants = []string{guest("Maya")}
			if err := calendarBusiness.UpdateEvent(ctx, uuid.MustParse(out.EventUuid), in, &sam); err != nil {
				t.Fatal(err)
			}
			title := sam.UserDgraphInfo.DisplayName() + " took you off an event"
			waitLong(t, "Bo's push", 20*time.Second, func() bool { return len(devicesPushed(title)) > 0 })
			time.Sleep(time.Second) // anything else on its way has landed
			if got := devicesPushed(title); !slices.Equal(got, []string{"tok-bo"}) {
				t.Errorf("the push went to %v, want Bo's phone alone: Paul is paused, Fay is in focus time, Dex is deactivated", got)
			}
			if got := noticedBy(broker, "event.uninvited|"+out.EventUuid+"|", people); !slices.Equal(got, []string{"Bo"}) {
				t.Errorf("the desktop notice reached %v, want Bo alone", got)
			}
			// The gate holds the push, not the telling: Paul and Fay find it
			// in their inbox. Dex is told nothing at all.
			for name, want := range map[string]int{"Bo": 1, "Paul": 1, "Fay": 1, "Dex": 0, "Maya": 0, "Sam": 0} {
				var n int
				if err := env.PG.QueryRow(`SELECT count(*) FROM resource_notices WHERE user_uuid = $1 AND kind = 'event.uninvited'`, id(name)).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != want {
					t.Errorf("%s has %d notices of being taken off, want %d", name, n, want)
				}
			}
		})

		t.Run("a booking page's owner", func(t *testing.T) {
			for _, owner := range []string{"Bo", "Paul", "Fay", "Dex"} {
				notificationBusiness.TellBookingOwner(ctx, notificationBusiness.BookingNotice{
					OwnerUUID: id(owner).String(), EventUUID: uuid.NewString(), BookingID: uuid.NewString(),
					GuestName: "Priya", PageTitle: "Office hours with " + owner,
					Start: start, End: start.Add(30 * time.Minute), TZ: "UTC", When: "Monday at 10:00",
				})
			}
			for owner, want := range map[string][]string{"Bo": {"tok-bo"}, "Paul": nil, "Fay": nil, "Dex": nil} {
				if got := devicesPushed("Priya booked Office hours with " + owner); !slices.Equal(got, want) {
					t.Errorf("the booking on %s's page was pushed to %v, want %v", owner, got, want)
				}
			}
		})
	})
}

// noticedBy is who of people was sent a desktop notice whose id starts with
// prefix.
func noticedBy(b *noticeBroker, prefix string, people map[string]userModels.UserInfo) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var who []string
	for name, p := range people {
		topic := helpers.GetMqttTopicForUserActivity(p.UserDgraphInfo.Uuid)
		for _, m := range b.got {
			var msg struct {
				Type int8                         `json:"type"`
				Data mqttStruct.MqttDesktopNotice `json:"data"`
			}
			if m.topic == topic && json.Unmarshal(m.payload, &msg) == nil &&
				msg.Type == mqttStruct.MESSAGE_DESKTOP_NOTICE && strings.HasPrefix(msg.Data.NoticeID, prefix) {
				who = append(who, name)
				break
			}
		}
	}
	slices.Sort(who)
	return who
}
