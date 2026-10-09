package business

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func at(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

// The rule itself: what existed when the demo was last seeded is the demo's.
func TestWhatExistedAtTheSeedingIsTheDemos(t *testing.T) {
	seeded := *at("2026-10-09T04:16:00Z")
	cases := []struct {
		name    string
		created *time.Time
		known   bool
		want    bool
	}{
		{"made by the seeder", at("2026-10-09T04:15:30Z"), true, true},
		{"restored from the golden snapshot", at("2026-09-14T10:00:00Z"), true, true},
		{"made at the very moment it finished", at("2026-10-09T04:16:00Z"), true, true},
		{"made by a visitor after", at("2026-10-09T09:30:00Z"), true, false},
		// Fails closed: a nuisance for a visitor, never a lost channel.
		{"never seeded on this server", at("2026-10-09T09:30:00Z"), false, true},
		{"no creation time on it", nil, true, true},
		{"a zero creation time", &time.Time{}, true, true},
	}
	for _, c := range cases {
		if got := isTheDemos(c.created, seeded, c.known); got != c.want {
			t.Errorf("%s: isTheDemos = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTheSeedingTimeIsReadAsWritten(t *testing.T) {
	want := time.Date(2026, 10, 9, 4, 16, 3, 120000000, time.UTC)
	if got, ok := parseSeededAt(" " + want.Format(time.RFC3339Nano) + "\n"); !ok || !got.Equal(want) {
		t.Errorf("parseSeededAt = %v, %v; want %v", got, ok, want)
	}
	for _, bad := range []string{"", "yesterday", "0001-01-01T00:00:00Z"} {
		if _, ok := parseSeededAt(bad); ok {
			t.Errorf("parseSeededAt(%q) was taken as a seeding time", bad)
		}
	}
}

// stubSeeding makes the demo seeded at seeded (or never, if zero) for one test.
func stubSeeding(t *testing.T, seeded time.Time) *int {
	t.Helper()
	reads := 0
	prev := readSeededAt
	readSeededAt = func() (time.Time, bool) {
		reads++
		return seeded, !seeded.IsZero()
	}
	forgetCache()
	t.Cleanup(func() {
		readSeededAt = prev
		forgetCache()
	})
	return &reads
}

func TestOnlyTheSharedVisitorOnTheDemoIsKeptFrom(t *testing.T) {
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")
	stubSeeding(t, *at("2026-10-09T04:16:00Z"))
	seededChannel, theirOwn := at("2026-10-09T04:15:00Z"), at("2026-10-09T11:00:00Z")

	t.Setenv("DEMO_MODE", "true")
	if !KeepsFromVisitor("Visitor@Demo.example", seededChannel) {
		t.Error("the shared visitor could archive the demo's own channel")
	}
	if KeepsFromVisitor("visitor@demo.example", theirOwn) {
		t.Error("the shared visitor could not archive a channel they made")
	}
	if KeepsFromVisitor("owner@company.example", seededChannel) {
		t.Error("the people who run the demo were kept from its content")
	}

	t.Setenv("DEMO_MODE", "")
	if KeepsFromVisitor("visitor@demo.example", seededChannel) {
		t.Error("a server that is not the demo kept something from a user")
	}
}

func TestNeverSeededKeepsEverythingFromTheVisitor(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")
	stubSeeding(t, time.Time{})
	if !KeepsFromVisitor("visitor@demo.example", at("2026-10-09T11:00:00Z")) {
		t.Error("with no record of a seeding, the visitor could archive anything")
	}
}

func TestTheSeedingTimeIsReadAtMostEveryHalfMinute(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")
	reads := stubSeeding(t, *at("2026-10-09T04:16:00Z"))
	for i := 0; i < 5; i++ {
		KeepsFromVisitor("visitor@demo.example", at("2026-10-09T11:00:00Z"))
	}
	if *reads != 1 {
		t.Errorf("read the seeding time %d times for five refusals", *reads)
	}
	// The read is not cached for anyone else: nobody else is ever asked about.
	KeepsFromVisitor("owner@company.example", nil)
	if *reads != 1 {
		t.Errorf("a non-visitor's request read the seeding time")
	}
}

// The seeder records the moment, or nothing here ever counts as the demo's
// after the first restore without a record.
func TestTheSeederRecordsWhenItFinished(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "services", "DemoSeed", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	run, mark := strings.Index(s, "demoBusiness.Run("), strings.Index(s, "demoGuard.MarkSeeded(")
	if run < 0 || mark < 0 || mark < run {
		t.Fatal("demoseed does not record when it finished, after the run")
	}
}

// Every way the visitor could archive or delete the demo's content asks
// first. The integration test (tests/integration/demo_seeded_test.go) proves
// the refusal on a real graph; this keeps a refactor from dropping a check
// without Docker.
func TestEveryArchiveAndDeleteAsksFirst(t *testing.T) {
	for file, act := range map[string]string{
		"controllers/Channel/channelController.go": "business.UpdateChannelInfo(ctx, &updateChannelNameInfo, channelUUID)",
		"controllers/Project/projectController.go": "business.ArchiveProjectByProjectUUID(ctx, projectUUID, dgraphProjectInfo)",
		"controllers/Doc/docController.go":         "business.DeleteDoc(ctx, docInfo.DocId)",
		"controllers/Team/teamController.go":       "business.ArchiveTeamByTeamUUID(ctx, teamUUID, dgraphTeam)",
	} {
		src, err := os.ReadFile(filepath.Join("..", "..", file))
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		i := strings.Index(s, act)
		if i < 0 {
			t.Fatalf("%s: %q is gone; update this test with where it went", file, act)
		}
		before := s[:i]
		guard := strings.LastIndex(before, "demoGuard.KeepsFromVisitor(")
		fn := strings.LastIndex(before, "\nfunc ")
		if guard < 0 || guard < fn {
			t.Errorf("%s: archives or deletes without asking demoGuard.KeepsFromVisitor first", file)
			continue
		}
		if !regexp.MustCompile(`"code":\s*"demo"`).MatchString(before[guard:]) {
			t.Errorf("%s: the refusal does not say code \"demo\", which the app words as the demo's", file)
		}
	}
}
