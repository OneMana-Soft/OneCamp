package business

import (
	"context"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// Is anybody draining the agent queue?
//
// A job that is never claimed never fails, never posts and never times out; it
// sits in 'queued' looking exactly like a job that is about to start. Before
// the server could be split into roles that was a rare state (the process was
// down, and everything else was too). Now it is one setting away: every
// replica started as SERVICE_ROLE=api drains nothing, and the workspace's AI
// teammates simply stop answering with no error anywhere. This is the check
// that says so, in the words an operator needs.
//
// unclaimedGrace is longer than any healthy gap between a job becoming due and
// a worker taking it (the worker ticks every five seconds, and a restart
// finishes well inside a minute), so it does not fire on ordinary latency.
const unclaimedGrace = 2 * time.Minute

// queueStall is the pure decision, separated so the boundary between "busy"
// and "abandoned" can be tested without a database.
//
// Overdue work with a live lease somewhere is a worker that is behind, which
// is a capacity question and healthy for this check's purposes. Overdue work
// with no live lease anywhere is work nobody will do.
func queueStall(s model.QueueClaimSummary, now time.Time) error {
	if s.Overdue == 0 || s.LiveLeases > 0 {
		return nil
	}
	waited := "for a while"
	if s.OldestDue != nil {
		waited = fmt.Sprintf("for %s", now.Sub(*s.OldestDue).Truncate(time.Second))
	}
	return fmt.Errorf("%d agent job(s) have been due %s and no worker has claimed any of them. "+
		"Nothing is draining the queue: check that at least one replica runs with SERVICE_ROLE unset or "+
		"set to worker, that it is up, and that AI is enabled", s.Overdue, waited)
}

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "agent-queue",
		Kind: helpers.CheckKindBehaviour,
		Describe: "Agent work that is due is being picked up by a worker. It does not say the work is " +
			"finishing quickly, only that something is taking it; a backlog behind a busy worker passes.",
		Probe: func(ctx context.Context) error {
			now := time.Now()
			s, err := model.SummariseUnclaimed(ctx, now.Add(-unclaimedGrace))
			if err != nil {
				return fmt.Errorf("could not read the agent queue: %w", err)
			}
			return queueStall(s, now)
		},
	})
}
