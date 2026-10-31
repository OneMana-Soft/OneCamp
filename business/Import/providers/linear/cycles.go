package linear

import (
	"context"
	"strconv"
	"strings"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// Linear's cycles (L-2) are a team's; a project here has its own. A project
// gets the cycles that hold its issues, with their names, dates and how they
// ended, and goes on making cycles as the team's settings say.

// cyclesQuery reads every team's cycles, closed ones too, in one list.
const cyclesQuery = `
query ($cursor: String) {
  cycles(first: 50, after: $cursor, includeArchived: true) {
    nodes { id number name startsAt endsAt completedAt team { id } }
    pageInfo { hasNextPage endCursor }
  }
}`

type linearCycle struct {
	ID          string     `json:"id"`
	Number      float64    `json:"number"`
	Name        string     `json:"name"`
	StartsAt    time.Time  `json:"startsAt"`
	EndsAt      time.Time  `json:"endsAt"`
	CompletedAt *time.Time `json:"completedAt"`
	Team        struct {
		ID string `json:"id"`
	} `json:"team"`
}

// fetchCycles reads every team's cycles, a page at a time.
func (p *Provider) fetchCycles(ctx context.Context, tok string) ([]linearCycle, error) {
	return rootList[linearCycle](ctx, p, tok, cyclesQuery, "cycles")
}

// name is the cycle's name as Linear shows it: its own, else "Cycle 12".
func (c linearCycle) name() string {
	if strings.TrimSpace(c.Name) != "" {
		return c.Name
	}
	return "Cycle " + strconv.Itoa(int(c.Number))
}

// projectCycles are the cycles that hold a project's issues, in the order
// they first come, and for a closed one how many of them it finished. A
// sub-issue comes over inside its parent's task, so only issues that become
// tasks count.
func projectCycles(issues []*linearIssue, byID map[string]linearCycle) []importProvider.SourceCycle {
	at := map[string]int{}
	var out []importProvider.SourceCycle
	for _, iss := range issues {
		c, ok := byID[iss.CycleID]
		if !ok || iss.ParentID != "" {
			continue
		}
		i, seen := at[c.ID]
		if !seen {
			i = len(out)
			at[c.ID] = i
			out = append(out, importProvider.SourceCycle{SourceID: c.ID, Name: c.name(), StartsAt: c.StartsAt, EndsAt: c.EndsAt, CompletedAt: c.CompletedAt})
		}
		if c.CompletedAt != nil && iss.CompletedAt != nil {
			out[i].Done++
		}
	}
	return out
}

// cycleSchedule is how a team makes its cycles, for a project here to go on
// making them; nil when the team's cycles are off. Linear moves a cycle's
// unfinished issues on to the next, and its cycles fall on the weeks of the
// one running now (else the last before now, else the first).
func cycleSchedule(t linearTeam, cycles []linearCycle, now time.Time) *importProvider.SourceCycleSchedule {
	if !t.CyclesEnabled || len(cycles) == 0 {
		return nil
	}
	var anchor, first time.Time
	for _, c := range cycles {
		if !c.StartsAt.After(now) && c.StartsAt.After(anchor) {
			anchor = c.StartsAt
		}
		if first.IsZero() || c.StartsAt.Before(first) {
			first = c.StartsAt
		}
	}
	if anchor.IsZero() {
		anchor = first
	}
	return &importProvider.SourceCycleSchedule{Weeks: t.CycleWeeks, CooldownWeeks: t.CooldownWeeks, Upcoming: t.Upcoming, RollOver: true, Anchor: anchor}
}
