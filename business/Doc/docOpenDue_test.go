package businness

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	accessModels "github.com/akashc777/OneCamp/models/postgres/Access"
)

// dueWorld stands in for the doc grants due to end (dueDocGrants), their
// generation (expiryWrites), the per-call ended-grant read (endedOn) and the
// clock (readersNow), counting each read. Di's view of every doc ended a
// second before noon, as the doc's side reads it (readersEnding).
type dueWorld struct {
	due        map[string]time.Time
	dueErr     error
	generation uint64
	ended      map[string]bool
	now        time.Time
	dueReads   int
	endedReads int
}

func withDue(t *testing.T, due map[string]time.Time) *dueWorld {
	t.Helper()
	withReaders(t, func() (*Sharing, error) { return readersWorld(), nil },
		[]*accessModels.Expiry{ends("user", rDi, noonR.Add(-time.Second))}, nil)
	// A generation of its own: what another test kept is never this one's.
	w := &dueWorld{due: due, generation: uint64(time.Now().UnixNano()), ended: map[string]bool{}, now: noonR}
	oldD, oldG, oldE, oldN := dueDocGrants, expiryWrites, endedOn, readersNow
	t.Cleanup(func() { dueDocGrants, expiryWrites, endedOn, readersNow = oldD, oldG, oldE, oldN })
	dueDocGrants = func(_ context.Context, by time.Time, limit int) (map[string]time.Time, error) {
		w.dueReads++
		if w.dueErr != nil {
			return nil, w.dueErr
		}
		out := map[string]time.Time{}
		for id, end := range w.due {
			if !end.After(by) && len(out) < limit {
				out[id] = end
			}
		}
		return out, nil
	}
	expiryWrites = func() uint64 { return w.generation }
	endedOn = func(_ context.Context, ids []string) (map[string]bool, error) {
		w.endedReads++
		out := map[string]bool{}
		for _, id := range ids {
			if w.ended[id] {
				out[id] = true
			}
		}
		return out, nil
	}
	readersNow = func() time.Time { return w.now }
	return w
}

// diOpens is Di's decision on a doc she was given view of, as a doc open
// (GetDocPage) decides it.
func diOpens(t *testing.T, ctx context.Context, doc string) level.Level {
	t.Helper()
	got, err := withEndedGrants(ctx, Viewer{UUID: rDi}, map[string]DocDecision{doc: {Level: level.View, Unlocked: level.View}})
	if err != nil {
		t.Fatalf("di opening %s: %v", doc, err)
	}
	return got[doc].Level
}

// TestASecondDocOpenReadsNoEndedGrantWhenNoneIsDue: a doc open reads which
// doc grants are due to end once, and the next open in the following 15 s
// reads nothing about grants that end (H6 A2-2); today every open reads the
// ended grants of its doc.
func TestASecondDocOpenReadsNoEndedGrantWhenNoneIsDue(t *testing.T) {
	w := withDue(t, nil)
	open := OpeningDoc(context.Background())
	for i := 0; i < 2; i++ {
		if got := diOpens(t, open, "plan"); got != level.View {
			t.Fatalf("open %d: di has %v; want view", i+1, got)
		}
	}
	w.now = noonR.Add(dueGrantsMaxAgeForTest - time.Millisecond)
	if got := diOpens(t, open, "notes"); got != level.View {
		t.Fatalf("a third open: di has %v; want view", got)
	}
	if w.endedReads != 0 || w.dueReads != 1 {
		t.Errorf("three opens read the ended grant %d times and the due grants %d times; want 0 and 1", w.endedReads, w.dueReads)
	}
}

// dueGrantsMaxAgeForTest is the 15 s a doc open keeps the due grants for.
const dueGrantsMaxAgeForTest = 15 * time.Second

// TestAnEndedGrantInTheDueSetClosesTheOpen: a doc whose grant has ended is
// decided again from its side, as before, whether the end was kept or read:
// Di can't open it in the minute before the sweep, also when her grant ends
// after the due grants were read; a grant still running leaves it open.
func TestAnEndedGrantInTheDueSetClosesTheOpen(t *testing.T) {
	w := withDue(t, map[string]time.Time{
		"ended": noonR.Add(-time.Second),
		"soon":  noonR.Add(5 * time.Second),
		"later": noonR.Add(time.Hour),
	})
	open := OpeningDoc(context.Background())
	if got := diOpens(t, open, "ended"); got != level.None {
		t.Errorf("di opened a doc whose grant ended: %v; want none", got)
	}
	if got := diOpens(t, open, "soon"); got != level.View {
		t.Errorf("di, whose grant ends in 5 s, has %v; want view", got)
	}
	if got := diOpens(t, open, "later"); got != level.View {
		t.Errorf("di, whose grant ends in an hour, has %v; want view", got)
	}
	w.now = noonR.Add(6 * time.Second)
	if got := diOpens(t, open, "soon"); got != level.None {
		t.Errorf("di opened a doc whose grant ended a second ago: %v; want none", got)
	}
	if w.dueReads != 1 || w.endedReads != 0 {
		t.Errorf("four opens read the due grants %d times and the ended grant %d times; want 1 and 0", w.dueReads, w.endedReads)
	}
}

// TestAWriteInTheWindowReadsTheEndedGrantFresh: only a doc open reads the
// kept due grants. Once an open kept "nothing is due", a grant ended where
// this process did not see it (another server instance) still closes every
// other decision, a write among them: they read the ended grant fresh.
func TestAWriteInTheWindowReadsTheEndedGrantFresh(t *testing.T) {
	w := withDue(t, nil)
	if got := diOpens(t, OpeningDoc(context.Background()), "plan"); got != level.View {
		t.Fatalf("di opening plan: %v", got)
	}
	w.ended["plan"] = true
	w.endedReads = 0
	if got := diOpens(t, context.Background(), "plan"); got != level.None {
		t.Errorf("a write after di's grant ended: di has %v; want none", got)
	}
	if w.endedReads != 1 {
		t.Errorf("the write read the ended grant %d times; want 1 (fresh)", w.endedReads)
	}
}

// TestTheKeptDueGrantsAreReadAgain: a write of any grant's end in this
// process, or 15 s, and the next open reads the due grants again; a read that
// fails fails the open and keeps nothing; more due docs than are kept and
// each open reads its own ended grants, as before.
func TestTheKeptDueGrantsAreReadAgain(t *testing.T) {
	t.Run("after a write", func(t *testing.T) {
		w := withDue(t, nil)
		open := OpeningDoc(context.Background())
		diOpens(t, open, "plan")
		w.generation++
		w.due = map[string]time.Time{"plan": noonR.Add(-time.Second)}
		if got := diOpens(t, open, "plan"); got != level.None || w.dueReads != 2 {
			t.Errorf("after a grant's end was written: di has %v after %d reads; want none after 2", got, w.dueReads)
		}
	})
	t.Run("after 15 s", func(t *testing.T) {
		w := withDue(t, nil)
		open := OpeningDoc(context.Background())
		diOpens(t, open, "plan")
		w.due = map[string]time.Time{"plan": noonR.Add(10 * time.Second)}
		w.now = noonR.Add(dueGrantsMaxAgeForTest + time.Second)
		if got := diOpens(t, open, "plan"); got != level.None || w.dueReads != 2 {
			t.Errorf("16 s on: di has %v after %d reads; want none after 2", got, w.dueReads)
		}
	})
	t.Run("a read that fails", func(t *testing.T) {
		w := withDue(t, nil)
		w.dueErr = errors.New("down")
		open := OpeningDoc(context.Background())
		if _, err := withEndedGrants(open, Viewer{UUID: rDi}, map[string]DocDecision{"plan": {Level: level.View}}); err == nil {
			t.Error("an unreadable expiry table opened the doc")
		}
		w.dueErr = nil
		diOpens(t, open, "plan")
		if w.dueReads != 2 {
			t.Errorf("the open after a failed read read the due grants %d times; want 2 (nothing kept)", w.dueReads)
		}
	})
	t.Run("more due docs than are kept", func(t *testing.T) {
		due := map[string]time.Time{}
		for i := 0; i < 600; i++ {
			due[fmt.Sprintf("doc-%d", i)] = noonR.Add(time.Second)
		}
		w := withDue(t, due)
		w.ended["plan"] = true
		open := OpeningDoc(context.Background())
		for i := 0; i < 2; i++ {
			if got := diOpens(t, open, "plan"); got != level.None {
				t.Errorf("open %d with too many due docs to keep: di has %v; want none", i+1, got)
			}
		}
		if w.endedReads != 2 || w.dueReads != 1 {
			t.Errorf("two opens read the ended grant %d times and the due grants %d times; want 2 and 1", w.endedReads, w.dueReads)
		}
	})
}

// TestOnlyADocOpenReadsTheKeptDueGrants: OpeningDoc is called by the doc open
// (GetDocPage) and by nothing else outside tests, so every write and every
// other decision reads the ended grants fresh.
func TestOnlyADocOpenReadsTheKeptDueGrants(t *testing.T) {
	var callers []string
	err := filepath.WalkDir("../..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "OpeningDoc(") && !strings.HasPrefix(line, "func OpeningDoc(") && !strings.HasPrefix(strings.TrimSpace(line), "//") {
				callers = append(callers, filepath.ToSlash(p))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 1 || !strings.HasSuffix(callers[0], "controllers/Doc/docChromeController.go") {
		t.Errorf("OpeningDoc is called from %v; want only GetDocPage (controllers/Doc/docChromeController.go): any other caller would read the kept due grants", callers)
	}
}
