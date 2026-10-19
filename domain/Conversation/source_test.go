package domain

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// swap sets *v to x for the test and puts it back.
func swap[T any](t *testing.T, v *T, x T) {
	t.Helper()
	was := *v
	*v = x
	t.Cleanup(func() { *v = was })
}

// freshSource forgets the kept answer and the comparison asked for, before
// and after the test, and asks again on every call.
func freshSource(t *testing.T) {
	t.Helper()
	forget := func() {
		forgetSource()
		askedMu.Lock()
		askedFor = ""
		askedMu.Unlock()
	}
	forget()
	t.Cleanup(forget)
	swap(t, &sourceRecheck, 0)
}

// startedAtForTest makes this process count as started at at.
func startedAtForTest(t *testing.T, at time.Time) {
	t.Helper()
	was := started.Load()
	started.Store(at.Unix())
	t.Cleanup(func() { started.Store(was) })
}

func TestTheIndexIsReadOnlyWhileItIsKnownToMatchTheGraph(t *testing.T) {
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name       string
		forced     string
		backfilled bool
		state      State
		have       bool
		index      bool
		why        string
	}{
		{"filled in and clean", "", true, State{Kind: StateClean, At: at}, true, true, "matches the graph"},
		{"clean, compared after this process started", "", true, State{Kind: StateClean, At: at.Add(time.Second)}, true, true, "matches the graph"},
		{"clean, compared before this process started", "", true, State{Kind: StateClean, At: at.Add(-time.Second)}, true, false, "began before this process started"},
		{"kept on the graph by the setting", "graph", true, State{Kind: StateClean, At: at}, true, false, SourceEnv + "=graph"},
		{"the setting in another case", " Graph ", true, State{Kind: StateClean, At: at}, true, false, SourceEnv},
		{"not filled in yet", "", false, State{Kind: StateClean, At: at}, true, false, "still being filled in"},
		{"filled in, never compared", "", true, State{}, false, false, "hasn't been compared"},
		{"a comparison repaired differences", "", true, State{Kind: StateDrift, At: at, Note: "2 repaired"}, true, false, "repaired differences"},
		{"a write failed", "", true, State{Kind: StateDirty, At: at, Note: "adding to channel c"}, true, false, "a write to the conversations index failed"},
		{"restored from a backup", "", true, State{Kind: StateDirty, At: at, Note: restoredNote}, true, false, "restored from a backup at 2026-10-16T09:00:00Z"},
	} {
		got := decide(c.forced, c.backfilled, c.state, c.have, at)
		if got.Index != c.index || !strings.Contains(got.Why, c.why) {
			t.Errorf("%s: got %+v, want index %v and a reason with %q", c.name, got, c.index, c.why)
		}
	}
}

func TestAStateIsReadBackAsItWasWritten(t *testing.T) {
	at := time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)
	for _, s := range []State{
		{Kind: StateClean, At: at},
		{Kind: StateDrift, At: at, Note: "3 repaired: channel c: 2, group g: 1"},
		{Kind: StateDirty, At: at, Note: "a | in the note"},
	} {
		got, ok := ParseState(s.String())
		if !ok || got != s {
			t.Errorf("%q read back as %+v (%v)", s.String(), got, ok)
		}
	}
	for _, bad := range []string{"", "clean", "clean|yesterday", "unknown|2026-10-16T09:30:00Z"} {
		if _, ok := ParseState(bad); ok {
			t.Errorf("%q read as a state", bad)
		}
	}
}

// The coordinator's rule (16 Oct): a comparison that finds the index and the
// graph apart sends decisions back to the graph at once, says so in the log,
// and a clean comparison brings them back to the index, said again.
func TestADriftSendsDecisionsBackToTheGraphAndSaysWhy(t *testing.T) {
	freshSource(t)
	now := time.Now()
	state := State{Kind: StateClean, At: now}.String()
	swap(t, &readBackfilled, func() (bool, error) { return true, nil })
	swap(t, &readState, func(context.Context) (string, bool, error) { return state, true, nil })
	var said []Choice
	swap(t, &logFlip, func(_ context.Context, c Choice) { said = append(said, c) })
	ctx := context.Background()

	if !AccessSource(ctx).Index {
		t.Fatal("a clean index isn't read")
	}
	if !AccessSource(ctx).Index || len(said) != 1 {
		t.Fatalf("a second answer from a clean index changed something: %+v", said)
	}

	state = State{Kind: StateDrift, At: now, Note: "1 repaired: channel c: 1"}.String()
	c := AccessSource(ctx)
	if c.Index {
		t.Fatal("decisions still read the index after a comparison repaired it")
	}
	if len(said) != 2 || said[1].Index || !strings.Contains(said[1].Why, "1 repaired") {
		t.Fatalf("the fall back wasn't said with its reason: %+v", said)
	}

	state = State{Kind: StateClean, At: now.Add(time.Hour)}.String()
	if !AccessSource(ctx).Index || len(said) != 3 || !said[2].Index {
		t.Fatalf("a clean comparison didn't bring decisions back to the index, said: %+v", said)
	}
}

func TestTheSettingKeepsEveryDecisionOnTheGraph(t *testing.T) {
	freshSource(t)
	t.Setenv(SourceEnv, "graph")
	swap(t, &readBackfilled, func() (bool, error) { return true, nil })
	swap(t, &readState, func(context.Context) (string, bool, error) {
		return State{Kind: StateClean, At: time.Now()}.String(), true, nil
	})
	if c := AccessSource(context.Background()); c.Index || !strings.Contains(c.Why, SourceEnv) {
		t.Fatalf("the setting didn't keep decisions on the graph: %+v", c)
	}
}

func TestAStateThatCantBeReadReadsTheGraph(t *testing.T) {
	freshSource(t)
	swap(t, &readBackfilled, func() (bool, error) { return true, nil })
	swap(t, &readState, func(context.Context) (string, bool, error) { return "", false, errors.New("connection refused") })
	if c := AccessSource(context.Background()); c.Index || !strings.Contains(c.Why, "connection refused") {
		t.Fatalf("an unreadable state: %+v", c)
	}
}

// A write that failed is heard at once by the process that marked it: the
// next decision reads the state again instead of the kept answer.
func TestAFailedWriteSendsThisProcessToTheGraphAtOnce(t *testing.T) {
	forgetSource()
	t.Cleanup(forgetSource)
	swap(t, &sourceRecheck, time.Hour)
	var state string
	swap(t, &readBackfilled, func() (bool, error) { return true, nil })
	swap(t, &readState, func(context.Context) (string, bool, error) { return state, state != "", nil })
	swap(t, &writeState, func(_ context.Context, v string) error { state = v; return nil })
	swap(t, &logFlip, func(context.Context, Choice) {})
	state = State{Kind: StateClean, At: time.Now()}.String()
	ctx := context.Background()
	if !AccessSource(ctx).Index {
		t.Fatal("a clean index isn't read")
	}
	MarkDirty(ctx, "adding to channel c:\n  connection reset")
	got, ok := ParseState(state)
	if !ok || got.Kind != StateDirty || got.Note != "adding to channel c: connection reset" {
		t.Fatalf("the failed write was recorded as %q", state)
	}
	if AccessSource(ctx).Index {
		t.Fatal("the process that marked the index dirty kept reading it")
	}
}

// A restored database brings back the "clean" of a comparison made before the
// backup, beside a newer graph. A process trusts no comparison that began
// before it started: it reads the graph, asks once for a comparison since its
// start, and reads the index again when one finds it clean.
func TestAProcessTrustsNoComparisonOlderThanItself(t *testing.T) {
	freshSource(t)
	start := time.Now().Add(-time.Minute).Truncate(time.Second)
	startedAtForTest(t, start)
	state := State{Kind: StateClean, At: start.Add(-time.Hour)}.String()
	swap(t, &readBackfilled, func() (bool, error) { return true, nil })
	swap(t, &readState, func(context.Context) (string, bool, error) { return state, true, nil })
	swap(t, &logFlip, func(context.Context, Choice) {})
	var asked []time.Time
	fail := true
	swap(t, &requestComparison, func(_ context.Context, at time.Time) error {
		asked = append(asked, at)
		if fail {
			return errors.New("connection reset")
		}
		return nil
	})
	ctx := context.Background()

	c := AccessSource(ctx)
	if c.Index || !strings.Contains(c.Why, "began before this process started") {
		t.Fatalf("a comparison from before the start is trusted: %+v", c)
	}
	// A request that couldn't be written is made again at the next read; one
	// that was is made once.
	fail = false
	AccessSource(ctx)
	AccessSource(ctx)
	if len(asked) != 2 || !asked[0].Equal(start) || !asked[1].Equal(start) {
		t.Fatalf("asked for comparisons after %v, want twice after %v (once failed)", asked, start)
	}

	// A comparison stamped at the start is since it.
	state = State{Kind: StateClean, At: start}.String()
	if c := AccessSource(ctx); !c.Index {
		t.Fatalf("a comparison since the start isn't trusted: %+v", c)
	}
	if len(asked) != 2 {
		t.Fatalf("a trusted state asked for a comparison: %v", asked)
	}
}

// A comparison is due after this process's start, or after a later start
// another process asked for, unless that one's clock is too far ahead.
func TestAComparisonIsDueAfterTheLatestStartAskedFor(t *testing.T) {
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	startedAtForTest(t, start)
	var asked time.Time
	var have bool
	swap(t, &readRequest, func(context.Context) (time.Time, bool, error) { return asked, have, nil })
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		asked time.Time
		have  bool
		want  time.Time
	}{
		{"nothing asked", time.Time{}, false, start},
		{"asked by a process older than this one", start.Add(-time.Minute), true, start},
		{"asked by a process started since", start.Add(30 * time.Minute), true, start.Add(30 * time.Minute)},
		{"asked from a clock an hour ahead", time.Now().Add(time.Hour), true, start},
	} {
		asked, have = c.asked, c.have
		got, err := ComparisonDueAfter(ctx)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%s: due after %v (%v), want %v", c.name, got, err, c.want)
		}
	}
	swap(t, &readRequest, func(context.Context) (time.Time, bool, error) {
		return time.Time{}, false, errors.New("connection refused")
	})
	if _, err := ComparisonDueAfter(ctx); err == nil {
		t.Error("a request that couldn't be read was taken for none")
	}
}

// What a comparison found is stamped with when it began, so a process that
// started while it ran doesn't trust it.
func TestASettledStateIsStampedWithWhenItsComparisonBegan(t *testing.T) {
	forgetSource()
	t.Cleanup(forgetSource)
	var wrote []string
	swap(t, &swapState, func(_ context.Context, from, to string) (bool, error) {
		if from != "dirty|x" {
			t.Errorf("swapped from %q", from)
		}
		wrote = append(wrote, to)
		return true, nil
	})
	began := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	if ok, err := Settle(context.Background(), "dirty|x", began, 0, ""); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, err := Settle(context.Background(), "dirty|x", began, 2, "channel c: 2"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	want := []string{"clean|2026-10-17T09:00:00Z|", "drift|2026-10-17T09:00:00Z|2 repaired: channel c: 2"}
	if strings.Join(wrote, " ") != strings.Join(want, " ") {
		t.Fatalf("settled %q, want %q", wrote, want)
	}
}
