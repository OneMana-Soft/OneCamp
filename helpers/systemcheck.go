package helpers

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"time"
)

// A registry of checks that answer "is this installation actually working".
//
// WHY THIS EXISTS. Unit tests were green through every serious defect this
// product has had. Entity links were broken from the day they shipped: linking a
// doc to a task silently did nothing and no linked doc ever appeared, and nothing
// anywhere said so. The GitHub link dialog had never once opened. The sync queue
// was failing most of its writes. In every case the code was correct in
// isolation, the seam between two subsystems was not, and nothing exercised the
// seam.
//
// A self-hoster stands up fifteen services and has no way to learn any of that.
// The onboarding checklist tells them what to DO. Nothing tells them what WORKS.
//
// READ-ONLY BY CONTRACT. A check probes; it never creates, mutates or deletes
// anything in somebody's workspace. Writing a real task to prove tasks work
// would be a better test and a worse product: an admin pressing "check my
// install" must not find test rows in their project afterwards. Each check
// therefore looks for the OBSERVABLE SIGNATURE a defect leaves behind, and says
// in its own description what it does and does not prove.
//
// An inversion for the same reason as the evidence pack next door: the runner
// lives in a package both editions ship, and some subsystems exist in only one.
// A check announces itself from its own package's init, so linking the package
// is what makes the check run.
// The two questions a check can answer, which an admin needs in this order.
//
// A dependency failing means the install does not work at all: files cannot be
// stored, messages do not arrive, search returns nothing. A behaviour failing
// means everything is reachable and one feature is nonetheless broken -- the
// shape of every defect this product has shipped.
//
// The distinction is not cosmetic. Without it, a FRESH install reads as
// perfectly healthy: it has no tasks, no syncs and no agent runs, so every
// behaviour check passes for want of anything to be wrong about, while MinIO has
// no bucket and no upload will ever succeed. Three green ticks on a broken
// install is exactly the reassurance this file was written to stop giving.
const (
	CheckKindDependency = "dependency"
	CheckKindBehaviour  = "behaviour"
)

type SystemCheck struct {
	// Name is the subsystem, as an admin would say it.
	Name string
	// Kind is CheckKindDependency or CheckKindBehaviour. Empty means behaviour,
	// which is the safe default: a dependency mislabelled as behaviour merely
	// sorts later, where the reverse would claim a working install is broken.
	Kind string
	// Describe says what this proves and what it does not, in one sentence,
	// because a green tick with no scope is worth less than nothing.
	Describe string
	// Probe returns nil when healthy, or an error describing what is wrong in
	// terms an operator can act on. It must not mutate anything.
	Probe func(ctx context.Context) error
}

// SystemCheckResult is one check's outcome.
type SystemCheckResult struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Describe string `json:"describe"`
	Healthy  bool   `json:"healthy"`
	Detail   string `json:"detail,omitempty"`
	TookMs   int64  `json:"took_ms"`
}

// SystemCheckNote is something an operator should know that is NOT a failure.
//
// Some true answers are neither pass nor fail. An install with no email key is
// not broken -- plenty never send mail -- but an admin about to invite their
// team needs telling that nothing will arrive. Reporting that as unhealthy would
// make a fresh, correct install show red for a deliberate choice, and a check
// that cries wolf is a check people stop reading.
//
// So a probe returns this instead of a plain error: the check stays healthy and
// the message is carried through to the page as a note.
type systemCheckNote struct{ msg string }

func (n systemCheckNote) Error() string { return n.msg }

// SystemCheckNote wraps a message a probe wants shown without failing.
func SystemCheckNote(msg string) error { return systemCheckNote{msg: msg} }

var (
	systemCheckMu sync.RWMutex
	systemChecks  = map[string]SystemCheck{}
)

// RegisterSystemCheck records a check. Call from package init.
//
// Re-registering a name replaces it, matching RegisterFeature: a duplicate is a
// programming error, and panicking would fail the whole server's boot over a
// diagnostic.
func RegisterSystemCheck(c SystemCheck) {
	if c.Name == "" || c.Probe == nil {
		return
	}
	if c.Kind != CheckKindDependency {
		c.Kind = CheckKindBehaviour
	}
	systemCheckMu.Lock()
	defer systemCheckMu.Unlock()
	systemChecks[c.Name] = c
}

// RunSystemChecks runs every registered check and returns one result each,
// ordered by name so two runs are comparable at a glance.
//
// A probe that panics is reported as unhealthy rather than taking the request
// down: the whole point is to be the thing that still answers when a subsystem
// is broken.
//
// Each probe gets its own timeout. One subsystem hanging must not stop an admin
// learning about the other eighteen, which is precisely the situation they are
// most likely to be in when they press this.
func RunSystemChecks(ctx context.Context, perCheckTimeout time.Duration) []SystemCheckResult {
	systemCheckMu.RLock()
	names := make([]string, 0, len(systemChecks))
	for name := range systemChecks {
		names = append(names, name)
	}
	checks := make([]SystemCheck, 0, len(names))
	sort.Strings(names)
	for _, n := range names {
		checks = append(checks, systemChecks[n])
	}
	systemCheckMu.RUnlock()

	// Dependencies first, then behaviour, each alphabetical. An admin reading
	// this wants "can this install work at all" before "is a feature broken",
	// and the order stays total, so two runs remain comparable at a glance.
	sort.SliceStable(checks, func(i, j int) bool {
		if (checks[i].Kind == CheckKindDependency) != (checks[j].Kind == CheckKindDependency) {
			return checks[i].Kind == CheckKindDependency
		}
		return checks[i].Name < checks[j].Name
	})

	if perCheckTimeout <= 0 {
		perCheckTimeout = 10 * time.Second
	}

	// Concurrently, because these are independent read-only probes against
	// different services and running them in a line makes the WORST case the sum
	// of every timeout. At ten checks and an eight second ceiling that is eighty
	// seconds for one request, which is past what a reverse proxy will hold and
	// far past what anyone waits for. Run together, the worst case is one
	// timeout.
	//
	// Each goroutine writes its own index, so no lock is needed and the order
	// established above survives. runOneCheck already contains its own recover,
	// which is what makes GoSafeNamed's guard a second line rather than the only
	// one -- an unrecovered panic in any goroutine takes the whole process down.
	out := make([]SystemCheckResult, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		i, c := i, c
		GoSafeNamed("systemcheck:"+c.Name, func() {
			defer wg.Done()
			out[i] = runOneCheck(ctx, c, perCheckTimeout)
		})
	}
	wg.Wait()
	return out
}

// runOneCheck isolates a single probe's failure, panic and timeout.
func runOneCheck(ctx context.Context, c SystemCheck, timeout time.Duration) (res SystemCheckResult) {
	started := time.Now()
	res = SystemCheckResult{Name: c.Name, Kind: c.Kind, Describe: c.Describe, Healthy: true}

	defer func() {
		res.TookMs = time.Since(started).Milliseconds()
		if r := recover(); r != nil {
			LogErrorWithContext(ctx, "system check %s panicked: %v\n%s", c.Name, r, debug.Stack())
			res.Healthy = false
			res.Detail = fmt.Sprintf("the check itself failed: %v", r)
		}
	}()

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := c.Probe(probeCtx); err != nil {
		var note systemCheckNote
		if errors.As(err, &note) {
			// Healthy, with something to say. See systemCheckNote.
			res.Detail = note.msg
		} else {
			res.Healthy = false
			res.Detail = err.Error()
		}
	}
	return res
}
