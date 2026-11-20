package helpers

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Transaction advisory locks are taken one way: pgexec.LockXact.
//
// WHY. A pg_advisory_xact_lock run outside a transaction is let go when its
// own statement ends, so it guards nothing, and each hand-written call trusted
// its caller to be inside one (the audit counted 14, REPORT D9). LockXact
// refuses outside InTx or WithTx (ErrNotInTx), so the lock a write asks for is
// the one it runs under. This fails on any hand-written advisory lock in
// production Go outside models/pgexec, in any form (handAdvisoryLock), but for
// the statements advisoryLocksKept lists by path and count.
func TestAdvisoryXactLocksGoThroughLockXact(t *testing.T) {
	var found []string
	kept := map[string][]string{}
	for _, path := range productionGoFiles(t) {
		if strings.HasPrefix(filepath.ToSlash(path), "../models/pgexec/") {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if handAdvisoryLock(line) {
				rel := strings.TrimPrefix(filepath.ToSlash(path), "../")
				at := fmt.Sprintf("%s:%d", rel, i+1)
				if _, ok := advisoryLocksKept[rel]; ok {
					kept[rel] = append(kept[rel], at)
					continue
				}
				found = append(found, at)
			}
		}
	}
	for rel, why := range advisoryLocksKept {
		if got := kept[rel]; len(got) != len(why) {
			t.Errorf("advisoryLocksKept lists %d statements in %s, which has %d (%s): a new one takes pgexec.LockXact, and a kept one gone drops its reason",
				len(why), rel, len(got), strings.Join(got, ", "))
		}
	}
	if len(found) > 0 {
		t.Errorf("%d hand-written advisory locks outside models/pgexec: take the lock with pgexec.LockXact(ctx, key), which refuses outside a transaction:\n  %s",
			len(found), strings.Join(found, "\n  "))
	}
}

// advisoryLocksKept are the hand-written advisory locks that stay, by path,
// one reason per statement: each takes a set of keys chosen in SQL in one
// statement, which LockXact (one key per call, exclusive, waiting) is not.
var advisoryLocksKept = map[string][]string{
	"models/postgres/DataTable/computedModel.go": {
		"ReachComputed tries every table's computedLock at once (bool_and(pg_try_advisory_xact_lock)) and never queues: a queued exclusive request would hold up every write behind it",
		"MarkComputedDirty holds its tables' and rows' tables' computedLock shared (pg_advisory_xact_lock_shared) over a set chosen in SQL",
	},
}

// handAdvisoryLockRe is every form of advisory lock Postgres has: session or
// transaction, try or wait, exclusive or shared, in any case, with any space
// before the parenthesis.
var handAdvisoryLockRe = regexp.MustCompile(`(?i)pg_(try_)?advisory_(xact_)?lock(_shared)?\s*\(`)

// handAdvisoryLock says whether line takes an advisory lock by hand.
func handAdvisoryLock(line string) bool { return handAdvisoryLockRe.MatchString(line) }

// TestAdvisoryLockClassifierExamples pins which forms count as a hand-written
// advisory lock: every form Postgres has (session or transaction, try or wait,
// exclusive or shared, any case, a space before the parenthesis), and no
// unlock or LockXact call.
func TestAdvisoryLockClassifierExamples(t *testing.T) {
	for _, c := range []struct {
		line string
		want bool
	}{
		{"SELECT pg_advisory_xact_lock(hashtext($1))", true},
		{"SELECT pg_advisory_xact_lock_shared(hashtext($1))", true},
		{"SELECT pg_try_advisory_xact_lock(hashtext($1))", true},
		{"SELECT pg_try_advisory_xact_lock_shared(hashtext($1))", true},
		{"SELECT pg_advisory_lock($1)", true},
		{"SELECT pg_advisory_lock_shared($1)", true},
		{"SELECT pg_try_advisory_lock($1)", true},
		{"SELECT pg_advisory_xact_lock (hashtext($1))", true},
		{"SELECT PG_ADVISORY_XACT_LOCK(hashtext($1))", true},
		{"SELECT pg_advisory_unlock($1)", false},
		{"SELECT pg_advisory_unlock_all()", false},
		{"if err := pgexec.LockXact(ctx, key); err != nil {", false},
	} {
		if got := handAdvisoryLock(c.line); got != c.want {
			t.Errorf("handAdvisoryLock(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}
