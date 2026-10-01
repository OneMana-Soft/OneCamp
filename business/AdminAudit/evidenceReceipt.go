package business

// Taking a receipt for each month's evidence pack, so the output accrues
// instead of being something somebody has to remember to fetch.
//
// WHY A JOB AND NOT A BUTTON. The pack is assembled from rows that still exist,
// and retention redacts row content once it passes the window. A pack built in
// March covering January is a weaker document than one built in February
// covering January, and the difference is silent: both say "verified", and the
// later one verified fewer rows and took more of them at their word. A workspace
// that only ever generates packs on demand cannot tell those apart afterwards.
//
// So each month is fingerprinted while its rows are intact. What accumulates is
// a chain of custody: fourteen receipts is fourteen points at which this log was
// what it says it was, and a regeneration that disagrees with one of them is a
// finding rather than a silence.
//
// EDITION-NEUTRAL. The pack's sections are contributed by whichever packages the
// build links, so this works unchanged on both editions and a receipt from the
// AI-free build simply has fewer sections in its manifest.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
)

// receiptCheckInterval is how often the job looks for a month it has not
// fingerprinted yet.
//
// DAILY, NOT MONTHLY. A monthly ticker assumes the process lives a month, and
// this one restarts on every deploy; a workspace that updates fortnightly would
// never take a receipt at all. Checking daily and doing nothing when the work is
// already done costs one indexed lookup a day.
const receiptCheckInterval = 24 * time.Hour

// receiptStartupDelay lets the stack finish coming up before the first check.
// Nothing here is urgent and the first minutes after boot are the busiest.
const receiptStartupDelay = 5 * time.Minute

// receiptBackfillMonths bounds how far back a first run will reach.
//
// A workspace that installs this after a year of operation should not spend that
// first tick building twelve packs, each a full chain verification. Twelve is
// also the answer to "how much history is worth anchoring retroactively", which
// is: the part still inside any plausible retention window. Older months are
// left alone rather than fingerprinted from redacted rows, because a receipt
// that says "verified 40 rows, took 8000 at their word" is worse than no receipt.
const receiptBackfillMonths = 12

// StartEvidenceReceipts runs the monthly fingerprint on a timer.
func StartEvidenceReceipts(ctx context.Context) {
	helpers.GoSafeNamed("evidence-receipts", func() {
		timer := time.NewTimer(receiptStartupDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		// The timer took these, with nobody watching. Said once here, so every
		// row the job writes below carries it. See initiator.go.
		ctx = WithInitiator(ctx, InitiatorSchedule)
		ticker := time.NewTicker(receiptCheckInterval)
		defer ticker.Stop()
		for {
			RecordDueReceipts(ctx, time.Now().UTC())
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

// RecordDueReceipts fingerprints every completed month that has no receipt yet,
// oldest first, and returns how many it wrote.
//
// `now` is a parameter rather than read inside, so the month arithmetic is
// testable without waiting for one.
func RecordDueReceipts(ctx context.Context, now time.Time) int {
	written := 0
	for _, p := range duePeriods(now, receiptBackfillMonths) {
		has, err := auditModel.HasReceiptFor(ctx, p.From, p.To)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "evidence receipts: checking %s: %+v", p.Label(), err)
			continue
		}
		if has {
			continue
		}
		if ok := recordReceipt(ctx, p); ok {
			written++
		}
	}
	return written
}

// Period is one closed month, half-open [From, To).
type Period struct {
	From time.Time
	To   time.Time
}

// Label names the period the way a person says it.
func (p Period) Label() string { return p.From.Format("January 2006") }

// duePeriods lists the completed months before `now`, oldest first.
//
// COMPLETED ONLY. The current month is deliberately absent: a receipt for a
// window that is still accepting rows would fingerprint a document nobody can
// reproduce, because the next row changes it.
//
// Pure, and takes both the clock and the depth, so the boundaries are tested
// rather than trusted. Month arithmetic through AddDate on the first of a month
// is the one form that does not skip: adding a month to the 31st lands in the
// month after next.
func duePeriods(now time.Time, months int) []Period {
	if months <= 0 {
		return nil
	}
	firstOfThis := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)

	out := make([]Period, 0, months)
	for i := months; i >= 1; i-- {
		start := firstOfThis.AddDate(0, -i, 0)
		out = append(out, Period{From: start, To: start.AddDate(0, 1, 0)})
	}
	return out
}

// recordReceipt builds the pack for a period and stores what it said.
//
// The pack itself is discarded. Keeping every month's full export would grow
// without bound and duplicate the log it was built from; the fingerprint and the
// manifest are what make a later regeneration checkable, and they are small.
func recordReceipt(ctx context.Context, p Period) bool {
	pack, err := BuildEvidencePack(ctx, p.From, p.To, "scheduled")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "evidence receipts: building %s: %+v", p.Label(), err)
		return false
	}

	if !worthAnchoring(pack) {
		return false
	}

	manifest, err := json.Marshal(pack.Integrity.Manifest)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "evidence receipts: manifest for %s: %+v", p.Label(), err)
		return false
	}

	r := &auditModel.EvidenceReceipt{
		PeriodStart:     p.From,
		PeriodEnd:       p.To,
		GeneratedAt:     pack.Pack.GeneratedAt,
		PackFingerprint: pack.Integrity.PackFingerprint,
		Manifest:        manifest,
	}
	if v := pack.Integrity.ChainVerification; v != nil {
		r.ChainOK, r.ChainChecked, r.ChainRedacted = v.OK, v.Checked, v.Redacted
	}

	wrote, err := auditModel.InsertReceiptIfAbsent(ctx, r)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "evidence receipts: storing %s: %+v", p.Label(), err)
		return false
	}
	if wrote {
		helpers.LogInfoWithContext(ctx, "evidence receipt taken for %s: %s", p.Label(), r.PackFingerprint)
		// On the record, as the workspace's own doing. Taking a receipt is the
		// kind of event the log exists to keep: an auditor asking when a month
		// was fingerprinted, and by whom, reads the answer here and not in a
		// process log that retention never touches.
		if err := RecordForPrincipal(ctx, nil, "", ActorSystem, "audit.evidence_receipt", CategorySecurity,
			fmt.Sprintf("Evidence receipt taken for %s: %d rows, chain %s", p.Label(), packRows(pack), chainWord(r.ChainOK)),
			map[string]interface{}{
				"period_start":     p.From.Format(time.RFC3339),
				"period_end":       p.To.Format(time.RFC3339),
				"pack_fingerprint": r.PackFingerprint,
				"rows":             packRows(pack),
				"chain_ok":         r.ChainOK,
			}); err != nil {
			helpers.LogErrorWithContext(ctx, "evidence receipts: audit row for %s: %+v", p.Label(), err)
		}
	}
	return wrote
}

// worthAnchoring decides whether a window's pack is worth a receipt.
//
// A MONTH WITH NOTHING IN IT IS NOT ANCHORED. Found by watching the first
// backfill on a real workspace: nine of twelve months had no rows, and each
// produced a receipt with an identical fingerprint, because the fingerprint
// digests the manifest and an empty manifest is the same empty manifest every
// time. Nine rows carrying no information, crowding out the three that carried
// all of it. There is nothing to prove about a month in which nothing happened,
// and a list that says otherwise is padding.
//
// Split from recordReceipt so the rule is tested without a database. The first
// version lived inline, and a mutation that deleted it passed every test.
func worthAnchoring(pack *EvidencePack) bool {
	return pack != nil && packRows(pack) > 0
}

// packRows totals what HAPPENED in the pack's window.
//
// From the manifest rather than the sections, so it counts what the document
// says it carries and stays correct for a section this function has never heard
// of. Distinct from the chain verification's count, which is a fact about the
// WHOLE log rather than about this window.
//
// CONTEXTUAL SECTIONS ARE NOT EVENTS. retention_policy describes the deployment
// and contributes exactly one row to every window forever, which made nine
// consecutive empty months look like months with something in them. The
// distinction is declared by the contributor and travels in the manifest, so a
// reader of the pack can make it without this code.
func packRows(pack *EvidencePack) int {
	if pack == nil {
		return 0
	}
	total := 0
	for _, m := range pack.Integrity.Manifest {
		if m.Contextual {
			continue
		}
		total += m.Rows
	}
	return total
}

// chainWord is the one word a summary uses for a verification result.
func chainWord(ok bool) string {
	if ok {
		return "verified"
	}
	return "NOT verified"
}

// ListReceipts is the read side, for the admin surface.
func ListReceipts(ctx context.Context, limit int) ([]*auditModel.EvidenceReceipt, error) {
	return auditModel.ListReceipts(ctx, limit)
}
