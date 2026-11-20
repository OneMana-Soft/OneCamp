package notification

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	weekHours "github.com/akashc777/OneCamp/business/WeekHours"
	emailQueueModels "github.com/akashc777/OneCamp/models/postgres/NotificationEmailQueue"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	emailService "github.com/akashc777/OneCamp/services/Email"
)

// Talk and work notices wait the window; what is due at a time, urgent,
// asked for or an access grant goes out as it is due.
func TestOnlyTalkAndWorkNoticesAreBatched(t *testing.T) {
	for _, k := range []EventType{
		EventChatDM, EventChannelMention, EventGroupChatMention, EventDocMention, EventTableMention, EventTaskMention,
		EventPostComment, EventChatComment, EventDocComment, EventTableComment, EventTaskComment,
		EventDocProposal, EventTaskAssigned, EventTaskStatus, EventProjectUpdate,
	} {
		if !batched(k) {
			t.Errorf("%s isn't batched", k)
		}
	}
	for _, k := range []EventType{
		EventDocShared, EventChannelCall, EventChatCall, EventMemoryDigest, EventCalendarBooking, EventCheckIn,
		EventCalendarInvited, EventCalendarUpdated, EventCalendarCancelled, EventCalendarAnswered,
		EventCalendarReminder, EventCalendarRecap, EventTaskUrgent, EventTaskSLA,
	} {
		if batched(k) {
			t.Errorf("%s is batched", k)
		}
	}
}

// A batched notice waits the window from now, then past the person's pause
// or quiet hours as they stand at its end; an unbatched one goes when it is
// due, and one asked for at once.
func TestABatchedNoticeWaitsTheWindow(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC) // a Wednesday
	later, soon := now.Add(2*time.Hour), now.Add(5*time.Minute)
	none := &prefModels.UserNotificationPreference{}
	paused := func(until time.Time) *prefModels.UserNotificationPreference {
		return &prefModels.UserNotificationPreference{NotificationsPausedUntil: &until}
	}
	start, end, tz := "10:10", "08:00", "UTC"
	nightly := &prefModels.UserNotificationPreference{QuietHoursEnabled: true, QuietHoursStart: &start, QuietHoursEnd: &end, QuietHoursTZ: &tz}
	tomorrowAtEight := time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	day := func(from, to string) []weekHours.Span { return []weekHours.Span{{Start: from, End: to}} }
	scheduled := &prefModels.UserNotificationPreference{Schedule: &prefModels.Schedule{TZ: "UTC", Week: map[string][]weekHours.Span{"3": day("09:00", "10:10"), "4": day("09:00", "17:00")}}}
	tomorrowAtNine := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		kind      EventType
		requested bool
		pref      *prefModels.UserNotificationPreference
		want      time.Time
	}{
		{"a DM waits the window", EventChatDM, false, none, now.Add(batchWindow)},
		{"a task's status waits the window", EventTaskStatus, false, none, now.Add(batchWindow)},
		{"a pause past the window wins", EventChatDM, false, paused(later), later},
		{"a pause ending inside the window waits for its end", EventPostComment, false, paused(soon), now.Add(batchWindow)},
		{"quiet hours starting inside the window wait for their end", EventChatDM, false, nightly, tomorrowAtEight},
		{"a schedule's range ending inside the window waits for the next", EventChatDM, false, scheduled, tomorrowAtNine},
		{"a reminder goes when due", EventCalendarReminder, false, none, now},
		{"an urgent task goes when due", EventTaskUrgent, false, none, now},
		{"a share goes when due", EventDocShared, false, none, now},
		{"a call keeps its pause", EventChatCall, false, paused(later), later},
		{"one asked for goes at once, in quiet hours too", EventChatDM, true, paused(later), now},
	}
	for _, c := range cases {
		got, deferred := dueAt(c.kind, c.requested, now, c.pref)
		if !got.Equal(c.want) || deferred != c.want.After(now) {
			t.Errorf("%s: got %v (deferred %v), want %v", c.name, got, deferred, c.want)
		}
	}
}

// A list re-claimed after its send (a close that didn't land, the reaper or
// a retry) is the same rows, so its key repeats and the provider sends it
// once: rows tried before never join the person's fresh ones.
func TestARetriedListKeepsItsKey(t *testing.T) {
	bo := uuid.New()
	at := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	row := func(min, attempts int) *emailQueueModels.QueueItem {
		return &emailQueueModels.QueueItem{ID: uuid.New(), UserID: bo, ToEmail: "bo@example.com", EventType: string(EventChatDM),
			Subject: "a notice", Attempts: attempts, CreatedAt: at.Add(time.Duration(min) * time.Minute)}
	}
	sh := shell{sender: "noreply@example.com"}
	a, b := row(0, 1), row(1, 1)
	first := groupsOf([]*emailQueueModels.QueueItem{a, b})
	if len(first) != 1 {
		t.Fatalf("the first claim is %d emails, want one", len(first))
	}
	sent, err := first[0].mail(sh)
	if err != nil {
		t.Fatal(err)
	}
	a.Attempts, b.Attempts = 2, 2
	fresh := row(5, 1)
	again := groupsOf([]*emailQueueModels.QueueItem{a, b, fresh})
	if len(again) != 2 {
		t.Fatalf("the re-claim is %d emails, want the list again and the fresh notice apart", len(again))
	}
	for _, g := range again {
		m, err := g.mail(sh)
		if err != nil {
			t.Fatal(err)
		}
		if retried := slices.Contains(g, a); retried && m.key != sent.key {
			t.Errorf("the re-sent list's key is %q, want the first send's %q", m.key, sent.key)
		} else if !retried && (len(g) != 1 || g[0] != fresh) {
			t.Errorf("the fresh notice went with %d rows, want alone", len(g))
		}
	}
}

// A batched notice's email, one or a list, keeps the day's reserve for what
// can't wait (NoticeReserve); any other email keeps none, as before.
func TestBatchedNoticesKeepTheReserve(t *testing.T) {
	row := func(kind EventType) *emailQueueModels.QueueItem {
		return &emailQueueModels.QueueItem{ID: uuid.New(), UserID: uuid.New(), ToEmail: "bo@example.com", EventType: string(kind), Subject: "a notice"}
	}
	sh := shell{sender: "noreply@example.com"}
	for _, c := range []struct {
		name string
		g    group
		want int
	}{
		{"a DM", group{row(EventChatDM)}, emailService.NoticeReserve},
		{"a list", group{row(EventChatDM), row(EventTaskStatus)}, emailService.NoticeReserve},
		{"a reminder", group{row(EventCalendarReminder)}, 0},
		{"a share", group{row(EventDocShared)}, 0},
		{"an urgent task", group{row(EventTaskUrgent)}, 0},
	} {
		m, err := c.g.mail(sh)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.g.options(m, sh).Keep; got != c.want {
			t.Errorf("%s keeps %d, want %d", c.name, got, c.want)
		}
	}
	if emailService.NoticeReserve == 0 {
		t.Error("NoticeReserve keeps nothing")
	}
}
