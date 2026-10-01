package helpers

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Each test gets the registry to itself.
func withCleanCheckRegistry(t *testing.T) {
	t.Helper()
	systemCheckMu.Lock()
	prev := systemChecks
	systemChecks = map[string]SystemCheck{}
	systemCheckMu.Unlock()
	t.Cleanup(func() {
		systemCheckMu.Lock()
		systemChecks = prev
		systemCheckMu.Unlock()
	})
}

func TestAHealthyProbeReportsHealthy(t *testing.T) {
	withCleanCheckRegistry(t)
	RegisterSystemCheck(SystemCheck{Name: "ok", Describe: "d", Probe: func(context.Context) error { return nil }})

	res := RunSystemChecks(context.Background(), time.Second)
	if len(res) != 1 || !res[0].Healthy {
		t.Fatalf("expected one healthy result, got %+v", res)
	}
}

// The operator has to be told WHAT is wrong, not just that something is.
func TestAFailingProbeCarriesItsReason(t *testing.T) {
	withCleanCheckRegistry(t)
	RegisterSystemCheck(SystemCheck{
		Name: "bad", Describe: "d",
		Probe: func(context.Context) error { return errors.New("the filter matches nothing") },
	})

	res := RunSystemChecks(context.Background(), time.Second)
	if len(res) != 1 || res[0].Healthy {
		t.Fatalf("expected one unhealthy result, got %+v", res)
	}
	if res[0].Detail != "the filter matches nothing" {
		t.Errorf("the reason was lost: %q", res[0].Detail)
	}
}

// THE POINT OF THE WHOLE THING. An admin presses this precisely when something
// is broken, so one broken probe must not stop the others being reported.
func TestOneBadProbeDoesNotHideTheOthers(t *testing.T) {
	withCleanCheckRegistry(t)
	RegisterSystemCheck(SystemCheck{Name: "a-panics", Describe: "d",
		Probe: func(context.Context) error { panic("subsystem is on fire") }})
	RegisterSystemCheck(SystemCheck{Name: "b-fine", Describe: "d",
		Probe: func(context.Context) error { return nil }})

	res := RunSystemChecks(context.Background(), time.Second)
	if len(res) != 2 {
		t.Fatalf("a panicking probe swallowed the others: got %d results", len(res))
	}
	byName := map[string]SystemCheckResult{}
	for _, r := range res {
		byName[r.Name] = r
	}
	if byName["a-panics"].Healthy {
		t.Error("a panicking probe reported healthy")
	}
	if !byName["b-fine"].Healthy {
		t.Error("a healthy probe was marked unhealthy because a different one panicked")
	}
}

// A hung subsystem is the case this exists for. It must time out, not block.
func TestAHangingProbeTimesOutAndTheRestStillRun(t *testing.T) {
	withCleanCheckRegistry(t)
	RegisterSystemCheck(SystemCheck{Name: "a-hangs", Describe: "d", Probe: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	RegisterSystemCheck(SystemCheck{Name: "b-fine", Describe: "d",
		Probe: func(context.Context) error { return nil }})

	done := make(chan []SystemCheckResult, 1)
	go func() { done <- RunSystemChecks(context.Background(), 50*time.Millisecond) }()

	select {
	case res := <-done:
		if len(res) != 2 {
			t.Fatalf("expected both checks reported, got %d", len(res))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hanging probe blocked the whole run, which is the situation this is for")
	}
}

// Two runs must be comparable at a glance.
func TestResultsAreOrderedByName(t *testing.T) {
	withCleanCheckRegistry(t)
	for _, n := range []string{"zebra", "alpha", "mike"} {
		name := n
		RegisterSystemCheck(SystemCheck{Name: name, Describe: "d",
			Probe: func(context.Context) error { return nil }})
	}

	res := RunSystemChecks(context.Background(), time.Second)
	if len(res) != 3 || res[0].Name != "alpha" || res[2].Name != "zebra" {
		t.Errorf("results are not ordered by name: %+v", res)
	}
}

// A check with no probe is a registration mistake and must not become a
// permanently green tick.
func TestAProbelessCheckIsNotRegistered(t *testing.T) {
	withCleanCheckRegistry(t)
	RegisterSystemCheck(SystemCheck{Name: "no-probe", Describe: "d"})

	if len(RunSystemChecks(context.Background(), time.Second)) != 0 {
		t.Error("a check with no probe was registered and would report healthy forever")
	}
}

// Dependencies come before behaviour, and the order stays total within each.
//
// The point is triage, not tidiness: an admin whose install has no MinIO bucket
// must not have to read past three feature checks to find that out.
func TestDependenciesAreReportedBeforeBehaviour(t *testing.T) {
	withCleanCheckRegistry(t)
	reg := func(name, kind string) {
		RegisterSystemCheck(SystemCheck{Name: name, Kind: kind, Describe: "d",
			Probe: func(context.Context) error { return nil }})
	}
	reg("zebra-dep", CheckKindDependency)
	reg("alpha-behaviour", CheckKindBehaviour)
	reg("beta-dep", CheckKindDependency)
	reg("yak-behaviour", CheckKindBehaviour)

	res := RunSystemChecks(context.Background(), time.Second)
	got := []string{}
	for _, r := range res {
		got = append(got, r.Name)
	}
	want := []string{"beta-dep", "zebra-dep", "alpha-behaviour", "yak-behaviour"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ordering: got %v, want %v", got, want)
		}
	}
}

// An unset Kind must not silently claim the install is broken.
func TestAnUnlabelledCheckIsTreatedAsBehaviour(t *testing.T) {
	withCleanCheckRegistry(t)
	RegisterSystemCheck(SystemCheck{Name: "unlabelled", Describe: "d",
		Probe: func(context.Context) error { return nil }})

	res := RunSystemChecks(context.Background(), time.Second)
	if len(res) != 1 || res[0].Kind != CheckKindBehaviour {
		t.Errorf("an unlabelled check must default to behaviour, got %+v", res)
	}
}

// A note is something worth saying that is not a failure.
//
// Without this, the only way to tell an admin that no email key is set would be
// to fail the check, which paints a fresh and deliberately mail-less install red
// and teaches people to ignore the page.
func TestANoteIsReportedWithoutFailingTheCheck(t *testing.T) {
	withCleanCheckRegistry(t)
	RegisterSystemCheck(SystemCheck{Name: "notes", Kind: CheckKindDependency, Describe: "d",
		Probe: func(context.Context) error { return SystemCheckNote("nothing is broken but you should know") }})

	res := RunSystemChecks(context.Background(), time.Second)
	if len(res) != 1 {
		t.Fatalf("expected one result, got %d", len(res))
	}
	if !res[0].Healthy {
		t.Error("a note must not fail the check")
	}
	if res[0].Detail != "nothing is broken but you should know" {
		t.Errorf("the note must reach the page, got %q", res[0].Detail)
	}
}

// A real failure must still fail, or the note mechanism would swallow errors.
func TestAPlainErrorStillFails(t *testing.T) {
	withCleanCheckRegistry(t)
	RegisterSystemCheck(SystemCheck{Name: "broken", Kind: CheckKindDependency, Describe: "d",
		Probe: func(context.Context) error { return errors.New("the bucket does not exist") }})

	res := RunSystemChecks(context.Background(), time.Second)
	if len(res) != 1 || res[0].Healthy {
		t.Errorf("a plain error must fail the check, got %+v", res)
	}
	if res[0].Detail != "the bucket does not exist" {
		t.Errorf("the reason must reach the page, got %q", res[0].Detail)
	}
}

// Probes run together, so the worst case is ONE timeout and not their sum.
//
// This is why it matters rather than being a micro-optimisation: at ten checks
// and an eight second ceiling, running in a line makes a fully broken install
// take eighty seconds to report -- past what a reverse proxy will hold open, and
// far past what anyone waits for. The install most in need of this page is
// exactly the one where every probe is slow.
func TestProbesRunConcurrently(t *testing.T) {
	withCleanCheckRegistry(t)

	const n = 8
	const each = 150 * time.Millisecond
	for i := 0; i < n; i++ {
		RegisterSystemCheck(SystemCheck{
			Name: fmt.Sprintf("slow-%02d", i), Describe: "d",
			Probe: func(ctx context.Context) error {
				select {
				case <-time.After(each):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
		})
	}

	started := time.Now()
	res := RunSystemChecks(context.Background(), 5*time.Second)
	elapsed := time.Since(started)

	if len(res) != n {
		t.Fatalf("expected %d results, got %d", n, len(res))
	}
	for _, r := range res {
		if !r.Healthy {
			t.Errorf("%s should have completed well inside its timeout: %s", r.Name, r.Detail)
		}
	}

	// Sequential would be n*each. Half of that is comfortably above one probe's
	// time and comfortably below the sum, so this is not timing-flaky while
	// still failing outright if the loop goes back to being serial.
	if limit := (n * each) / 2; elapsed > limit {
		t.Errorf("checks appear to run one after another: %d probes of %s took %s (limit %s)",
			n, each, elapsed, limit)
	}
}
