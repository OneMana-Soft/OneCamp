package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

func TestDayOfIsTheDayACycleEdgeFallsOnWhereItWasMade(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	ny := time.FixedZone("EST", -5*3600)
	for _, c := range []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), "2026-10-19"},
		{time.Date(2026, 10, 19, 0, 0, 0, 0, ist), "2026-10-19"}, // 18:30 UTC the day before
		{time.Date(2026, 10, 19, 0, 0, 0, 0, ny), "2026-10-19"},  // 05:00 UTC
	} {
		if got := dayOf(c.at); got != c.want {
			t.Errorf("dayOf(%v) = %s, want %s", c.at, got, c.want)
		}
	}
}

func TestCyclesICSWritesEachCycleAsAFreeAllDayEvent(t *testing.T) {
	now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	start := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	done := start.AddDate(0, 0, 14)
	cycles := []View{
		{Cycle: cycleModel.Cycle{Id: uuid.New(), Number: 1, StartsAt: start.AddDate(0, 0, -14), EndsAt: start, CompletedAt: &done}, Progress: Progress{Total: 5, Done: 5}},
		{Cycle: cycleModel.Cycle{Id: uuid.New(), Number: 2, Name: "Polish, then ship", StartsAt: start, EndsAt: start.AddDate(0, 0, 14)}, Progress: Progress{Total: 9, Done: 4}},
		{Cycle: cycleModel.Cycle{Id: uuid.New(), Number: 3, StartsAt: start.AddDate(0, 0, 14), EndsAt: start.AddDate(0, 0, 28)}},
	}
	out := string(CyclesICS("Launch", cycles, now))
	for _, want := range []string{
		"X-WR-CALNAME:Launch cycles",
		"SUMMARY:Launch: Cycle 1", "DESCRIPTION:Completed. 5 of 5 tasks done",
		`SUMMARY:Launch: Polish\, then ship`, "DTSTART;VALUE=DATE:20261019", "DTEND;VALUE=DATE:20261102", "DESCRIPTION:4 of 9 tasks done",
		"SUMMARY:Launch: Cycle 3", "DESCRIPTION:No tasks yet",
		"TRANSP:TRANSPARENT",
		"UID:cycle-" + cycles[1].Id.String() + "@onecamp",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the feed has no %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "BEGIN:VEVENT"); n != 3 {
		t.Errorf("%d events, want 3", n)
	}
}

// stubFeed sets the feed's seams for one test: a live link for person to
// project, the person's graph uid, and their standing.
func stubFeed(t *testing.T, person, project uuid.UUID, uid string, standing workAccess.Standing, allowed bool) string {
	t.Helper()
	raw, hash, err := helpers.NewLinkSecret()
	if err != nil {
		t.Fatal(err)
	}
	saved := struct {
		allowed  func(context.Context) bool
		byHash   func(context.Context, []byte) (*cycleModel.Feed, error)
		projects func(context.Context, []string, string) ([]*dgraphStruct.DgraphProject, error)
		stand    func(context.Context, string, []*dgraphStruct.DgraphProject) (map[string]workAccess.Standing, error)
		cycles   func(context.Context, uuid.UUID, time.Time) ([]View, error)
	}{allowed: feedsAllowed, byHash: feedByHash, projects: feedProjects, stand: feedStanding, cycles: feedCycles}
	savedUID := uidOf
	t.Cleanup(func() {
		feedsAllowed, feedByHash, feedProjects, feedStanding, feedCycles, uidOf = saved.allowed, saved.byHash, saved.projects, saved.stand, saved.cycles, savedUID
	})
	feedsAllowed = func(context.Context) bool { return allowed }
	feedByHash = func(_ context.Context, h []byte) (*cycleModel.Feed, error) {
		if string(h) != string(hash) {
			return nil, nil
		}
		return &cycleModel.Feed{ID: uuid.New(), ProjectUUID: project, PersonID: person, TokenHash: hash}, nil
	}
	uidOf = func(context.Context, accessBusiness.Subject) (string, error) {
		if uid == "gone" {
			// The graph has no live node for them: GetActiveDgraphUserInfoByUUID's answer.
			return "", helpers.NotFound("failed to get dgraph user")
		}
		return uid, nil
	}
	feedProjects = func(_ context.Context, ids []string, u string) ([]*dgraphStruct.DgraphProject, error) {
		if len(ids) != 1 || ids[0] != project.String() || u != uid {
			t.Fatalf("read projects %v for %q", ids, u)
		}
		return []*dgraphStruct.DgraphProject{{Uuid: project.String(), Name: "Launch"}}, nil
	}
	feedStanding = func(_ context.Context, who string, _ []*dgraphStruct.DgraphProject) (map[string]workAccess.Standing, error) {
		if who != person.String() {
			t.Fatalf("standing for %q", who)
		}
		standing.ProjectUUID = strings.ToLower(project.String())
		return map[string]workAccess.Standing{standing.ProjectUUID: standing}, nil
	}
	feedCycles = func(context.Context, uuid.UUID, time.Time) ([]View, error) {
		start := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
		return []View{{Cycle: cycleModel.Cycle{Id: uuid.New(), Number: 1, StartsAt: start, EndsAt: start.AddDate(0, 0, 14)}}}, nil
	}
	return raw
}

func TestFeedICSAnswersOnlyWhileItsPersonMayViewTheProject(t *testing.T) {
	person, project := uuid.New(), uuid.New()
	now := time.Now()
	member := workAccess.Standing{Role: workAccess.RoleMember}
	for _, c := range []struct {
		name     string
		uid      string
		standing workAccess.Standing
		allowed  bool
		token    func(raw string) string
		ok       bool
	}{
		{"a member's link", "0x1", member, true, func(r string) string { return r }, true},
		{"a viewer's link", "0x1", workAccess.Standing{Role: workAccess.RoleViewer}, true, func(r string) string { return r }, true},
		{"links turned off in the workspace", "0x1", member, false, func(r string) string { return r }, false},
		{"a made-up link", "0x1", member, true, func(string) string { return "made-up" }, false},
		{"an empty link", "0x1", member, true, func(string) string { return "" }, false},
		{"a person who left the workspace", "", member, true, func(r string) string { return r }, false},
		{"a person the graph has no live node for", "gone", member, true, func(r string) string { return r }, false},
		{"a person no longer in the project", "0x1", workAccess.Standing{Role: workAccess.RoleNone}, true, func(r string) string { return r }, false},
		{"an archived project", "0x1", workAccess.Standing{Role: workAccess.RoleAdmin, Archived: true}, true, func(r string) string { return r }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := stubFeed(t, person, project, c.uid, c.standing, c.allowed)
			body, err := FeedICS(context.Background(), c.token(raw), now)
			if c.ok {
				if err != nil || !strings.Contains(string(body), "SUMMARY:Launch: Cycle 1") {
					t.Fatalf("got %v:\n%s", err, body)
				}
				return
			}
			if !errors.Is(err, ErrFeedNotFound) || body != nil {
				t.Fatalf("got %v and %d bytes, want ErrFeedNotFound and nothing", err, len(body))
			}
		})
	}
}

func TestMakeFeedRefusedWhenTheWorkspaceTurnedLinksOff(t *testing.T) {
	saved, savedMake := feedsAllowed, makeFeed
	defer func() { feedsAllowed, makeFeed = saved, savedMake }()
	feedsAllowed = func(context.Context) bool { return false }
	makeFeed = func(context.Context, cycleModel.Feed) error {
		t.Fatal("made a link the workspace turned off")
		return nil
	}
	var ce *CycleError
	if _, err := MakeFeed(context.Background(), uuid.New(), uuid.New()); !errors.As(err, &ce) {
		t.Fatalf("got %v, want the person's error", err)
	}
}

func TestMakeFeedKeepsOnlyTheHashAndGivesTheLinkOnce(t *testing.T) {
	saved, savedMake := feedsAllowed, makeFeed
	defer func() { feedsAllowed, makeFeed = saved, savedMake }()
	feedsAllowed = func(context.Context) bool { return true }
	var kept cycleModel.Feed
	makeFeed = func(_ context.Context, f cycleModel.Feed) error { kept = f; return nil }
	project, person := uuid.New(), uuid.New()
	v, err := MakeFeed(context.Background(), project, person)
	if err != nil || !v.Live || !strings.Contains(v.URL, "/public/cycles/") || !strings.HasSuffix(v.URL, ".ics") {
		t.Fatalf("got %+v, %v", v, err)
	}
	raw := FeedTokenOf(v.URL[strings.LastIndex(v.URL, "/")+1:])
	if kept.ProjectUUID != project || kept.PersonID != person || string(kept.TokenHash) != string(helpers.LinkSecretHash(raw)) || strings.Contains(string(kept.TokenHash), raw) {
		t.Fatalf("kept %+v for the link %s", kept, raw)
	}
}
