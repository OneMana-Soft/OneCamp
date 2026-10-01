package business

// Retention for the detailed records a workspace keeps: the audit log here, and
// whatever else registers itself (the agent run ledger does, on the edition that
// has one).
//
// Nothing ever deleted an audit entry, which satisfies the AI Act's six-month
// minimum by never expiring and is the wrong default for a deployer who has to
// delete on a schedule. Retention was a database administration task, which
// means in practice it was nobody's.
//
// It REDACTS rather than deletes, because each entry hashes the previous one's
// hash: removing a row breaks verification for everything after it, and the
// chain is the reason the log is worth keeping. Clearing content while keeping
// the row satisfies the erasure requirement and leaves the integrity one
// standing. See migration 150.

import (
	"context"
	"time"

	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/helpers"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
)

func init() {
	// The retention policy in effect belongs in every evidence pack.
	//
	// A reviewer reading a pack finds rows whose content is gone and rows the
	// chain verification "took at their word". Neither is interpretable without
	// knowing what the policy was, and the policy is a setting that changes, so
	// recording it alongside the records is the only way the pack stays readable
	// a year later. Published research on evidence packages calls this the
	// policy version in effect, and lists it under required input context.
	//
	// It is also, deliberately, a section BOTH editions contribute. The registry
	// exists so the AI packages can add a section the AI-free build does not
	// have; if the only registrant were the agent ledger, the seam would have no
	// caller at all on that edition, which is the dead-code guard's whole point.
	helpers.RegisterEvidenceContributor(helpers.EvidenceSection{
		Name: "retention_policy",
		Describe: "The retention window in effect when this pack was built, and the stores it applies to. " +
			"Without it, a cleared record and a record that never existed look the same to a reader.",
		// The policy, not the month. It is the same one row whether the window
		// held ten thousand records or none, so it must not make an empty month
		// look like one with something in it.
		Contextual: true,
		Collect: func(ctx context.Context, _, _ time.Time) (any, error) {
			days := RetentionDays()
			stores := []string{}
			for _, s := range helpers.RetentionSweepers() {
				stores = append(stores, s.Name)
			}
			return map[string]any{
				"window_days":        days,
				"keeps_everything":   days <= 0,
				"minimum_days_floor": minRetentionDays,
				"swept_stores":       stores,
				"note": "Retention redacts rather than deletes: the row and its hashes remain, so the chain still " +
					"verifies, and a redacted row is reported separately from a checked one.",
			}, nil
		},
	})

	// Registered rather than called directly by the sweep below, even though it
	// is the same package. One code path for every store beats a hardcoded case
	// plus a loop, and it keeps the seam honest: if the audit log were the
	// exception, the registry would be something only AI code uses.
	helpers.RegisterRetentionSweeper("audit log",
		"rows and hashes kept, so the chain still verifies",
		auditModel.RedactOlderThan)
}

// retentionSweepInterval is how often the sweep runs. Retention is measured in
// months, so once a day is frequent enough for the window to be accurate to
// within a rounding error nobody can observe.
const retentionSweepInterval = 24 * time.Hour

// minRetentionDays mirrors the settings package's floor so local references and
// the evidence pack keep reading one value.
//
// The AI Act requires automatically generated logs to be kept for at least six
// months. Letting an operator configure less would turn a compliance control
// into a way to fail one by accident, so a shorter value is refused and the
// floor is used instead.
const minRetentionDays = settingsBusiness.MinRetentionDays

// RetentionDays is the configured window, resolved by the settings layer.
//
// It used to read AUDIT_RETENTION_DAYS here, with its own parsing and its own
// floor. That made two definitions of one policy, and the admin interface would
// have become a third. The setting owns it now: workspace value first,
// environment second, floor applied once.
func RetentionDays() int {
	return settingsBusiness.RetentionDays()
}

// StartRetentionSweep runs the redaction sweep on a timer. Inert when no window
// is configured, and it re-reads the setting every tick so turning retention on
// does not need a restart.
func StartRetentionSweep(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(retentionSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runRetentionSweep(ctx)
			}
		}
	}()
}

func runRetentionSweep(ctx context.Context) {
	days := RetentionDays()
	if days <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -days)

	// Every store that announced itself, including this package's own audit log.
	// The sweep does not know what it is sweeping: the agent run ledger lives in
	// the AI packages, which only one edition ships, and calling it from here
	// would not compile on the other one.
	//
	// Each store is swept independently, so one failing store cannot leave the
	// rest untouched for another day.
	for _, sweeper := range helpers.RetentionSweepers() {
		n, err := sweeper.Sweep(ctx, cutoff)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "retention sweep for %s failed: %+v", sweeper.Name, err)
			continue
		}
		if n == 0 {
			continue
		}
		// Worth a line even though it is routine: someone asking why an old
		// record has no detail should find the answer here rather than
		// concluding the trail was tampered with. Hence the note, which says
		// what survived.
		helpers.LogInfoWithContext(ctx,
			"retention: redacted %d %s record(s) older than %d days; %s", n, sweeper.Name, days, sweeper.Note)
	}
}
