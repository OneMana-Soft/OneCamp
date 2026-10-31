//go:build integration
// +build integration

package integration_test

// DG12's budget runners (lines below): a meeting's node written, and the
// two-step panel around an all-hands. The all-hands is the viewer's meeting
// with every one of the fixture's people it can invite (219 in the small
// tier), its notes linking the backlinked doc: written to the calendar's
// store once, in a warm-up, and given its node by the nightly pass, as a
// restore would be. Its node is with the first 200 invited, the cap the
// guests' read keeps on Postgres 12.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	refsBusiness "github.com/akashc777/OneCamp/business/Refs"
	workGraphDomain "github.com/akashc777/OneCamp/domain/WorkGraph"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration/dgfixture"
)

// allHandsWith is how many people a meeting's node is with at most
// (business/Calendar maxWith).
const allHandsWith = 200

var allHands string

// allHandsPeople is the fixture's first n people who can be invited:
// members, not the viewer, nobody deleted.
func allHandsPeople(f *dgfixture.Fixture, n int) []string {
	var out []string
	for _, p := range f.People {
		if len(out) == n {
			break
		}
		if !p.Bot && !p.External && !p.Deleted && p.UUID != f.Viewer.UUID {
			out = append(out, p.UUID)
		}
	}
	return out
}

// dg12AllHands is the all-hands' id, made the first time.
func dg12AllHands(ctx context.Context, f *dgfixture.Fixture) (string, error) {
	if allHands != "" {
		return allHands, nil
	}
	db := postgresInit.DBConn.SqlDB
	id, calendar := uuid.New(), uuid.New()
	start := time.Date(2026, 10, 26, 16, 0, 0, 0, time.UTC)
	steps := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO system_configs (key, value, updated_at) VALUES ('calendar_store_moved', 'done: the budget fixture', NOW())
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, nil},
		{`INSERT INTO calendars (id, kind, owner_id, tz) VALUES ($1, 'personal', $2, 'UTC')
			ON CONFLICT (owner_id) WHERE kind = 'personal' AND deleted_at IS NULL DO NOTHING`, []any{calendar, f.Viewer.UUID}},
		{`INSERT INTO events (id, calendar_id, kind, title, notes, start_at, end_at, tz, created_by, ical_uid)
			SELECT $1, id, 'event', 'All hands', $3, $4, $5, 'UTC', $2, $6 FROM calendars WHERE owner_id = $2 AND kind = 'personal' AND deleted_at IS NULL`,
			[]any{id, f.Viewer.UUID, "Agenda: https://onecamp.example/app/doc/" + f.BacklinkedDoc, start, start.Add(time.Hour), id.String() + "@onecamp"}},
	}
	for _, s := range steps {
		if _, err := db.ExecContext(ctx, s.q, s.args...); err != nil {
			return "", fmt.Errorf("the all-hands: %w", err)
		}
	}
	invited := allHandsPeople(f, len(f.People))
	if len(invited) <= allHandsWith {
		return "", fmt.Errorf("the fixture has %d people to invite: the cap needs more than %d", len(invited), allHandsWith)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO event_attendees (event_id, user_id, added_at)
		SELECT $1, u, NOW() + n * interval '1 second' FROM unnest($2::uuid[]) WITH ORDINALITY AS g(u, n)`, id, pq.Array(invited)); err != nil {
		return "", fmt.Errorf("the all-hands' guests: %w", err)
	}
	reps, err := refsBusiness.ReconcileAll(ctx, refsBusiness.Options{Apply: true, Kinds: []workGraphDomain.Kind{workGraphDomain.KindEvent}})
	if err != nil || len(reps) != 1 || reps[0].Drifted != 1 {
		return "", fmt.Errorf("the all-hands' node: %+v (%v)", reps, err)
	}
	nodes, err := workGraphDomain.ReadNodes(ctx, workGraphDomain.KindEvent, []string{id.String()})
	if n := nodes[id.String()]; err != nil || n == nil || len(n.Have["cal_event_with"]) != allHandsWith {
		return "", fmt.Errorf("the all-hands' node isn't with its first %d invited: %+v (%v)", allHandsWith, n, err)
	}
	allHands = id.String()
	return allHands, nil
}

// DG12's lines, apart from the table so packages adding lines there merge
// clean. Measured 22 Oct 2026 (calibrated p95, loads 33 and 16): nodes.write
// 13.8 and 7.3 ms; graph.event.2 21.1 ms, 6 trips, the all-hands' node with
// 200 of 219 invited. Each budget is about three times the worst, never
// under the floor.
func init() {
	dgraphBudgets = append(dgraphBudgets,
		dgraphBudget{"nodes.write", "small", budgetFloor, 0, 1, "DG12: a new meeting's node with its 200 people and its doc, one upsert by uuids"},                                   // 13.8
		dgraphBudget{"graph.event.2", "small", 70 * time.Millisecond, 0, 6, "DG13: the all-hands' panel two steps out: its 200 people capped, the backlinked doc and what links it"}, // 21.1
	)
}

func init() {
	// A new meeting's node: one upsert finds it, its 200 people and its doc
	// by their uuids and writes them.
	budgetRunners["nodes.write"] = func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		people := allHandsPeople(f, allHandsWith)
		with := make([]workGraphDomain.Ref, len(people))
		for i, p := range people {
			with[i] = workGraphDomain.Ref{Kind: workGraphDomain.KindPerson, UUID: p}
		}
		fix, _, err := workGraphDomain.NodeDiff(workGraphDomain.KindEvent, uuid.NewString(), nil, &workGraphDomain.Projection{Changed: time.Now(), Edges: map[string][]workGraphDomain.Ref{
			"cal_event_organiser":    {{Kind: workGraphDomain.KindPerson, UUID: f.Viewer.UUID}},
			"cal_event_with":         with,
			workGraphDomain.RefersTo: {{Kind: workGraphDomain.KindDoc, UUID: f.BacklinkedDoc}},
		}})
		if err != nil {
			return 0, err
		}
		return 1, workGraphDomain.WriteNodes(ctx, []workGraphDomain.NodeFix{fix})
	}
	// The all-hands' panel, two steps out, as its organiser sees it.
	budgetRunners["graph.event.2"] = func(ctx context.Context, f *dgfixture.Fixture) (int, error) {
		id, err := dg12AllHands(ctx, f)
		if err != nil {
			return 0, err
		}
		return graphConnected("event", func(*dgfixture.Fixture) string { return id }, 2)(ctx, f)
	}
}
