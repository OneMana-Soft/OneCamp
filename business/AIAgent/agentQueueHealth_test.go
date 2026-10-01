package business

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

func TestAnEmptyQueueIsHealthy(t *testing.T) {
	if err := queueStall(model.QueueClaimSummary{}, time.Now()); err != nil {
		t.Fatalf("nothing due means nothing to claim: %v", err)
	}
}

func TestABacklogBehindABusyWorkerIsCapacityNotAFault(t *testing.T) {
	// Six jobs waiting while three run under live leases is a worker that is
	// behind. Reporting that as "nobody is claiming" would send the operator
	// looking for a dead process that is alive and working.
	s := model.QueueClaimSummary{Overdue: 6, LiveLeases: 3}
	if err := queueStall(s, time.Now()); err != nil {
		t.Fatalf("a busy worker must pass this check: %v", err)
	}
}

func TestOverdueWorkWithNoLiveLeaseIsAQueueNobodyDrains(t *testing.T) {
	// The state the role split makes possible with one setting: due work,
	// and not one job running anywhere.
	due := time.Now().Add(-10 * time.Minute)
	s := model.QueueClaimSummary{Overdue: 2, OldestDue: &due, LiveLeases: 0}
	err := queueStall(s, time.Now())
	if err == nil {
		t.Fatal("due work with no worker claiming must fail the check")
	}
	for _, want := range []string{"2 agent job(s)", "SERVICE_ROLE", "AI is enabled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message must tell the operator what to check; missing %q in %q", want, err)
		}
	}
	if !strings.Contains(err.Error(), "10m0s") {
		t.Errorf("the message should say how long the oldest job has waited, got %q", err)
	}
}

func TestTheQueueCheckIsRegisteredWhereAnAdminWillSeeIt(t *testing.T) {
	// The decision above is only worth anything if the check runs. init()
	// registers it; RunSystemChecks is what the admin page calls. With no
	// database in a unit test the probe reports an error, which is fine: the
	// point is that a result named agent-queue comes back at all.
	results := helpers.RunSystemChecks(context.Background(), 2*time.Second)
	for _, r := range results {
		if r.Name == "agent-queue" {
			if r.Describe == "" {
				t.Fatal("the check must say what it proves")
			}
			return
		}
	}
	t.Fatal("agent-queue is not registered; the admin page would never show it")
}
