package aicoworker

import (
	"context"
	"strings"
	"testing"
	"time"

	aiAdapter "github.com/akashc777/OneCamp/adapter/AI"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func withNoteSeams(t *testing.T, gates bool, items []aiAdapter.AttentionItem) (claims *[]time.Time, posted *[]string) {
	t.Helper()
	sg, sc, sq, sb, sp, sn, sl, ss := noteGates, noteClaimDay, noteQueue, noteBot, notePost, noteNow, noteLeftOn, noteShared
	t.Cleanup(func() {
		noteGates, noteClaimDay, noteQueue, noteBot, notePost, noteNow, noteLeftOn, noteShared = sg, sc, sq, sb, sp, sn, sl, ss
	})
	noteShared = func(*userModels.UserInfo) bool { return false }
	var c []time.Time
	var p []string
	claimed := map[string]bool{}
	noteGates = func(context.Context) bool { return gates }
	noteClaimDay = func(_ context.Context, _ uuid.UUID, day time.Time) (bool, error) {
		c = append(c, day)
		k := day.Format("2006-01-02")
		if claimed[k] {
			return false, nil
		}
		claimed[k] = true
		return true, nil
	}
	noteQueue = func(context.Context, *userModels.UserInfo) (*aiAdapter.AttentionResponse, error) {
		return &aiAdapter.AttentionResponse{Enabled: true, Items: items}, nil
	}
	noteBot = func(context.Context) *userBusiness.BotIdentity { return &userBusiness.BotIdentity{UUID: "bot-1"} }
	notePost = func(_ context.Context, _ *userBusiness.BotIdentity, _ *userModels.UserInfo, h string) error {
		p = append(p, h)
		return nil
	}
	noteNow = func() time.Time { return time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC) }
	noteLeftOn = func(_ context.Context, _ uuid.UUID, day time.Time) (bool, error) {
		return claimed[day.Format("2006-01-02")], nil
	}
	return &c, &p
}

func TestNoteLinksStayInTheApp(t *testing.T) {
	h := composeNote("Sam", []aiAdapter.AttentionItem{overdue}, time.UTC, time.Now())
	// A path the client routes in place (useInternalLinkRouter), never a full
	// URL that would reload the app.
	if !strings.Contains(h, `<a href="/app/task/t1">`) {
		t.Fatalf("the note must link with an in-app path:\n%s", h)
	}
	if strings.Contains(h, "<small>") {
		t.Fatal("the chat renderer has no <small>; it rendered at full size anyway")
	}
}

func member() *userModels.UserInfo {
	u := &userModels.UserInfo{}
	u.UserPostgresInfo.Id = uuid.New()
	u.UserDgraphInfo.UserName = "Sam Rivera"
	return u
}

var overdue = aiAdapter.AttentionItem{Source: "task", Kind: "Overdue task", Title: "Ship <b>v2</b>", Subtitle: "Due yesterday", URL: "/app/task/t1"}

func TestTheNoteIsLeftOnceADay(t *testing.T) {
	_, posted := withNoteSeams(t, true, []aiAdapter.AttentionItem{overdue})
	u := member()
	first, err := LeaveDailyNote(context.Background(), u, "2026-09-27", "")
	if err != nil || !first.Posted || first.BotUUID != "bot-1" || first.Items != 1 {
		t.Fatalf("first open = %+v, %v", first, err)
	}
	again, _ := LeaveDailyNote(context.Background(), u, "2026-09-27", "")
	if again.Posted || len(*posted) != 1 {
		t.Fatalf("a second open the same day posted again: %+v", again)
	}
	next, _ := LeaveDailyNote(context.Background(), u, "2026-09-28", "")
	if !next.Posted {
		t.Fatal("the next day's open left no note")
	}
}

func TestNothingToSayNothingSent(t *testing.T) {
	_, posted := withNoteSeams(t, true, nil)
	res, _ := LeaveDailyNote(context.Background(), member(), "2026-09-27", "")
	if res.Posted || len(*posted) != 0 {
		t.Fatal("an empty queue produced a note")
	}
}

func TestOffMeansOff(t *testing.T) {
	claims, posted := withNoteSeams(t, false, []aiAdapter.AttentionItem{overdue})
	res, _ := LeaveDailyNote(context.Background(), member(), "2026-09-27", "")
	if res.Posted || len(*posted) != 0 || len(*claims) != 0 {
		t.Fatal("with AI or the coworker off, the day was claimed or a note posted")
	}
}

func TestTheMembersOwnDayIsUsedOnlyWhenPlausible(t *testing.T) {
	now := time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)
	for in, want := range map[string]string{
		"2026-09-26": "2026-09-26", // west of UTC, still yesterday there
		"2026-09-28": "2026-09-28", // far east, already tomorrow
		"2026-10-15": "2026-09-27", // a wrong clock does not buy extra notes
		"nonsense":   "2026-09-27",
	} {
		if got := noteDay(in, now).Format("2006-01-02"); got != want {
			t.Errorf("noteDay(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestTheNoteReadsWellAndEscapes(t *testing.T) {
	items := []aiAdapter.AttentionItem{overdue}
	for i := 0; i < 6; i++ {
		items = append(items, aiAdapter.AttentionItem{Kind: "Approval", Title: "Approve a post", URL: "javascript:alert(1)"})
	}
	h := composeNote("Sam", items, time.UTC, time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC))
	for _, want := range []string{"Hi Sam. 7 things need you today:", `<a href="/app/task/t1">Ship &lt;b&gt;v2&lt;/b&gt;</a>`, "Overdue task · Due yesterday", "And 2 more", "Turn it off"} {
		if !strings.Contains(h, want) {
			t.Errorf("note missing %q:\n%s", want, h)
		}
	}
	if strings.Contains(h, "javascript:") || strings.Contains(h, "<b>v2") {
		t.Fatalf("the note carried markup or a script link through:\n%s", h)
	}
	if strings.Count(h, "<li>") != noteMaxItems {
		t.Fatalf("the note lists %d items, want %d", strings.Count(h, "<li>"), noteMaxItems)
	}
	if one := composeNote("", []aiAdapter.AttentionItem{overdue}, time.UTC, time.Now()); !strings.HasPrefix(one, "<p>Hi. One thing needs you today:") {
		t.Fatalf("got %s", one)
	}
}

func TestEveryDemoVisitorFindsTodaysNote(t *testing.T) {
	_, posted := withNoteSeams(t, true, []aiAdapter.AttentionItem{overdue})
	noteShared = func(*userModels.UserInfo) bool { return true }
	u := member()
	_, _ = LeaveDailyNote(context.Background(), u, "2026-09-27", "")
	later, _ := LeaveDailyNote(context.Background(), u, "2026-09-27", "")
	if !later.Posted || len(*posted) != 1 {
		t.Fatalf("a later visitor = %+v with %d notes posted; want pointed at the one note", later, len(*posted))
	}
}

func TestDueTimesAreSaidInTheMembersZone(t *testing.T) {
	now := time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC) // 08:30 in Kolkata
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	for due, want := range map[string]string{
		"2026-09-27T17:00:00Z": "due today at 10:30 PM",
		"2026-09-28T04:00:00Z": "due tomorrow at 9:30 AM",
		"2026-09-25T10:00:00Z": "overdue since Fri, Sep 25",
		"2026-10-03T10:00:00Z": "due Sat, Oct 3",
		"not a time":           "",
	} {
		if got := duePhrase(due, kolkata, now); got != want {
			t.Errorf("duePhrase(%s) = %q, want %q", due, got, want)
		}
	}
	if helpers.Location("Not/AZone") != time.UTC || helpers.Location("") != time.UTC {
		t.Fatal("an unknown zone must fall back to UTC")
	}
	item := aiAdapter.AttentionItem{Kind: "Due soon", Title: "Write the launch announcement", Subtitle: "Due Sep 27, 5:00 PM · Q4 launch",
		DueTime: "2026-09-27T17:00:00Z", Context: "Q4 launch", URL: "/app/task/t"}
	h := composeNote("Sam", []aiAdapter.AttentionItem{item}, kolkata, now)
	if !strings.Contains(h, "Write the launch announcement</a> · due today at 10:30 PM · Q4 launch") || strings.Contains(h, "5:00 PM") {
		t.Fatalf("the note must say the member's time once, not the server's:\n%s", h)
	}
}
