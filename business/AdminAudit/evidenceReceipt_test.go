package business

import (
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// The receipt job's whole correctness is which windows it considers due. Getting
// that wrong is not a visible failure: it writes a receipt for a month that is
// still accepting rows, or silently never writes one at all, and either way
// every screen stays green.

func TestOnlyCompletedMonthsAreDue(t *testing.T) {
	// Mid-September. The current month is still accepting rows, so a receipt for
	// it would fingerprint a document the next row invalidates.
	now := time.Date(2026, 9, 17, 13, 0, 0, 0, time.UTC)
	periods := duePeriods(now, 3)

	if len(periods) != 3 {
		t.Fatalf("expected 3 periods, got %d", len(periods))
	}
	last := periods[len(periods)-1]
	if !last.To.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the newest window runs to %v; it must stop at the start of the current month", last.To)
	}
	if last.From.Month() != time.August {
		t.Errorf("the newest completed month should be August, got %v", last.From.Month())
	}
}

func TestPeriodsAreOldestFirstAndContiguous(t *testing.T) {
	// Oldest first so a first run fills history in the order it happened, and
	// contiguous so no month can fall between two receipts unnoticed.
	now := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	periods := duePeriods(now, 4)

	for i := 1; i < len(periods); i++ {
		if !periods[i].From.Equal(periods[i-1].To) {
			t.Errorf("gap between %v and %v", periods[i-1].To, periods[i].From)
		}
		if !periods[i].From.After(periods[i-1].From) {
			t.Errorf("periods are not oldest-first at %d", i)
		}
	}
}

// TestMonthArithmeticSurvivesTheLongMonths is the bug this shape avoids. Adding
// a month to the 31st lands in the month after next, so any arithmetic anchored
// on "today" skips months. Anchoring on the first of the month does not.
func TestMonthArithmeticSurvivesTheLongMonths(t *testing.T) {
	// 31 March: the previous month is February, which has no 31st.
	now := time.Date(2026, 3, 31, 23, 59, 0, 0, time.UTC)
	periods := duePeriods(now, 2)

	if len(periods) != 2 {
		t.Fatalf("expected 2 periods, got %d", len(periods))
	}
	if periods[0].From.Month() != time.January || periods[1].From.Month() != time.February {
		t.Errorf("expected January then February, got %v then %v",
			periods[0].From.Month(), periods[1].From.Month())
	}
	if periods[1].To.Month() != time.March {
		t.Errorf("February's window should end at the start of March, got %v", periods[1].To)
	}
}

func TestEveryWindowIsHalfOpenAndUTC(t *testing.T) {
	// Half-open, so the boundary row belongs to exactly one month. In UTC, so a
	// receipt taken from a server in one timezone covers the same rows as one
	// taken from another.
	for _, p := range duePeriods(time.Now(), 3) {
		if p.From.Location() != time.UTC || p.To.Location() != time.UTC {
			t.Errorf("window %v is not in UTC", p)
		}
		if p.From.Day() != 1 || p.To.Day() != 1 {
			t.Errorf("window %v does not run from the first to the first", p)
		}
		if !p.To.After(p.From) {
			t.Errorf("window %v does not move forward", p)
		}
	}
}

func TestNoBackfillIsAskedForWhenDepthIsZero(t *testing.T) {
	// A guard against a misconfigured depth turning the first tick into a year
	// of full chain verifications.
	if got := duePeriods(time.Now(), 0); len(got) != 0 {
		t.Errorf("expected no periods at depth 0, got %d", len(got))
	}
	if got := duePeriods(time.Now(), -3); len(got) != 0 {
		t.Errorf("expected no periods at a negative depth, got %d", len(got))
	}
}

func TestLabelReadsLikeAPersonSaysIt(t *testing.T) {
	p := Period{From: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	if got := p.Label(); got != "August 2026" {
		t.Errorf("label is %q", got)
	}
}

// TestPackRowsTotalsEverySection covers the count the receipt reports for a
// window, which is NOT the chain verification's count: that one is a fact about
// the whole log and is the same number on every month's receipt.
func TestPackRowsTotalsEverySection(t *testing.T) {
	pack := &EvidencePack{Integrity: PackIntegrity{Manifest: []ManifestEntry{
		{Section: "audit_log", Rows: 40},
		{Section: "agent_actions", Rows: 2},
	}}}
	if got := packRows(pack); got != 42 {
		t.Errorf("expected 42 rows across the sections, got %d", got)
	}
}

// TestAnEmptyWindowCountsAsNothingToAnchor is the finding from the first
// backfill: nine of twelve months had no rows, and each produced a receipt with
// an identical fingerprint, because the fingerprint digests the manifest and an
// empty manifest is the same one every time. Nine rows carrying no information,
// crowding out the three that carried all of it.
// TestAContextualSectionIsNotAnEvent is the bug the first backfill actually had.
// retention_policy describes the deployment, not the month, and contributes
// exactly one row to every window forever. Counting it made nine consecutive
// empty months look like months with something in them, each anchored with an
// identical fingerprint.
func TestAContextualSectionIsNotAnEvent(t *testing.T) {
	quietMonth := &EvidencePack{Integrity: PackIntegrity{Manifest: []ManifestEntry{
		{Section: "audit_log", Rows: 0},
		{Section: "retention_policy", Rows: 1, Contextual: true},
	}}}
	if got := packRows(quietMonth); got != 0 {
		t.Errorf("a month whose only row is the retention policy counted %d", got)
	}
	if worthAnchoring(quietMonth) {
		t.Error("a month with nothing but the policy description was anchored")
	}

	busyMonth := &EvidencePack{Integrity: PackIntegrity{Manifest: []ManifestEntry{
		{Section: "audit_log", Rows: 40},
		{Section: "retention_policy", Rows: 1, Contextual: true},
	}}}
	if got := packRows(busyMonth); got != 40 {
		t.Errorf("the policy row leaked into the window count: %d", got)
	}
	if !worthAnchoring(busyMonth) {
		t.Error("a month with forty records was not anchored")
	}
}

func TestAnEmptyWindowCountsAsNothingToAnchor(t *testing.T) {
	empty := &EvidencePack{Integrity: PackIntegrity{Manifest: []ManifestEntry{
		{Section: "audit_log", Rows: 0},
		{Section: "agent_actions", Rows: 0},
	}}}
	if got := packRows(empty); got != 0 {
		t.Errorf("a window with no rows totalled %d", got)
	}
	// No manifest at all is the same answer, not a panic.
	if got := packRows(&EvidencePack{}); got != 0 {
		t.Errorf("a pack with no manifest totalled %d", got)
	}

	// And the decision itself, which is what the job acts on.
	if worthAnchoring(empty) {
		t.Error("a month with no rows was judged worth anchoring")
	}
	if worthAnchoring(nil) {
		t.Error("a pack that failed to build was judged worth anchoring")
	}
	if !worthAnchoring(&EvidencePack{Integrity: PackIntegrity{
		Manifest: []ManifestEntry{{Section: "audit_log", Rows: 1}},
	}}) {
		t.Error("a month with a single row was not judged worth anchoring")
	}
}

// TestTheRetentionSectionDeclaresItselfContextual pins the declaration itself,
// not just the rule that reads it. The rule is only as good as the sections that
// use it, and this is the section the rule was written for: it contributes one
// row to every window forever, so a build where it stopped saying so would go
// straight back to anchoring every quiet month.
func TestTheRetentionSectionDeclaresItselfContextual(t *testing.T) {
	for _, s := range helpers.EvidenceContributors() {
		if s.Name != "retention_policy" {
			continue
		}
		if !s.Contextual {
			t.Error("retention_policy no longer declares itself contextual, so every quiet " +
				"month will be anchored again")
		}
		return
	}
	t.Fatal("retention_policy is not registered as an evidence section")
}

// TestTheReceiptJobRunsAsUnattendedWork pins that the job says so before it
// writes anything: it is the one caller of the initiator on this edition, and a
// receipt the timer took must not be indistinguishable in the log from one a
// person exported.
func TestTheReceiptJobRunsAsUnattendedWork(t *testing.T) {
	if !InitiatorSchedule.Unattended() {
		t.Fatal("a scheduled receipt must count as nobody watching")
	}
	if chainWord(true) != "verified" || chainWord(false) != "NOT verified" {
		t.Error("the summary word for the chain drifted")
	}
}
