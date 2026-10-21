package business

import (
	"context"
	"testing"
)

// RELEASE-LINE ONLY (v2.75.0 / v1.60.0). A doc's views by day are read only by
// docs C6-3's insights, and a member turns them off with "Don't record my
// views"; neither is in this release's web. So the release lines write no day
// rows, for members or a published page's visitors, and "Viewed by" is recorded
// as in v2.74.2. Beta writes them; the release that carries C6-3's web drops
// this test and turns them on again.
func TestTheReleaseLineWritesNoViewsByDay(t *testing.T) {
	viewed, days := fakeViews(t, true)
	recordsByDay = recordsByDayOnTheReleaseLine
	if err := RecordView(context.Background(), ResourceDoc, "d1", "u1"); err != nil {
		t.Fatal(err)
	}
	if len(*viewed) != 1 {
		t.Fatalf("Viewed by must still be recorded: %v", *viewed)
	}
	if err := CountPageVisit(context.Background(), ResourceDoc, "d1", "page", Visit{Address: "203.0.113.7", UserAgent: "Mozilla/5.0 (X11; Linux x86_64) Firefox/131.0"}); err != nil {
		t.Fatal(err)
	}
	if len(*days) != 0 {
		t.Fatalf("the release line wrote views by day: %v", *days)
	}
}
