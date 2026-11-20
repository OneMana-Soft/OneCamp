package domain

import (
	"strings"
	"testing"

	restrictionDomain "github.com/akashc777/OneCamp/domain/TaskRestriction"
)

// The queue's list, the cap's count and a project read's counts are one query
// over what waits: a second copy of what "waiting" means could drift from
// the queue the count stands for.
func TestTheProjectReadsCountIsTheQueuesOwnQuery(t *testing.T) {
	if !strings.Contains(waitingIn, "project_uuid = $1") || !strings.Contains(waitingIn, "decided_at IS NULL") {
		t.Fatalf("what waits is one project's undecided items: %s", waitingIn)
	}
	queries := map[string]string{"WaitingList": WaitingList, "CountWaiting": CountWaiting, "CountAwake": CountAwake, "CountAwakeFor": CountAwakeFor}
	for name, q := range queries {
		if strings.Count(q, waitingIn) != 1 || strings.Count(q, "FROM triage_items") != 1 {
			t.Errorf("%s doesn't read what waits by the one predicate: %s", name, q)
		}
		// Every condition past the project's is ANDed on at the top: an OR
		// there would reach past the project (CR-355's mutant).
		if top := topLevel(q); strings.Contains(strings.ToUpper(top), " OR ") {
			t.Errorf("%s has an OR outside parentheses: %s", name, top)
		}
	}
	if !strings.HasPrefix(CountAwake, CountWaiting+" AND (") || !strings.Contains(CountAwake, "snoozed_until IS NULL OR snoozed_until <= NOW()") {
		t.Errorf("CountAwake is the queue's count less what's snoozed now: %s", CountAwake)
	}
	if strings.Contains(CountWaiting, "snoozed") || strings.Contains(WaitingList, "snoozed_until <=") {
		t.Errorf("the queue lists and caps what waits snoozed or not: %s / %s", WaitingList, CountWaiting)
	}
	// A reader's count leaves out what a restriction's rows keep them ($2)
	// out of, by the restrictions' own predicate, not a copy of it.
	if !strings.HasPrefix(CountAwakeFor, CountAwake+" AND NOT EXISTS (") || !strings.Contains(CountAwakeFor, restrictionDomain.KeptOut("$2::uuid")) ||
		!strings.Contains(CountAwakeFor, "r.task_id = triage_items.task_uuid") {
		t.Errorf("CountAwakeFor is CountAwake less the restricted tasks kept from $2: %s", CountAwakeFor)
	}
}

// topLevel is q with every parenthesised part taken out.
func topLevel(q string) string {
	var b strings.Builder
	depth := 0
	for _, c := range q {
		switch {
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0:
			b.WriteRune(c)
		}
	}
	return b.String()
}
