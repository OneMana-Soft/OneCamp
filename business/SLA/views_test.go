package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	model "github.com/akashc777/OneCamp/models/postgres/SLA"
	"github.com/google/uuid"
)

// viewWorld is a project of four timed tasks (one on track, one at risk, one
// breached, one met) read by reader, a member when member is true, with the
// project's SLAs on when on is true.
func viewWorld(t *testing.T, reader string, member, guest, on bool) map[State]string {
	now := time.Now()
	ids := map[State]string{}
	var timers []model.Timer
	add := func(s State, state string, due time.Duration) {
		id := uuid.New()
		ids[s] = id.String()
		d, r := now.Add(due), now.Add(due-6*time.Hour)
		timers = append(timers, model.Timer{TaskID: id, ProjectID: project, State: state, DueAt: &d, RiskAt: &r})
	}
	add(OnTrack, model.Running, 48*time.Hour)
	add(AtRisk, model.Running, 2*time.Hour)
	add(Breached, model.Running, -time.Hour)
	add(Met, model.Met, -time.Hour)
	swap(t, &readerOf, func(context.Context) string { return reader })
	swap(t, &projectTimers, func(_ context.Context, p uuid.UUID) ([]model.Timer, error) {
		if p != project {
			return nil, nil
		}
		return timers, nil
	})
	swap(t, &membersAmong, func(_ context.Context, asked map[string][]string) (map[string]map[string]bool, error) {
		out := map[string]map[string]bool{}
		for p := range asked {
			out[p] = map[string]bool{reader: member}
		}
		return out, nil
	})
	swap(t, &isGuest, func(context.Context, string) bool { return guest })
	swap(t, &onIn, func(_ context.Context, projects []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, p := range projects {
			out[p] = on
		}
		return out, nil
	})
	return ids
}

func TestTheSLAFilterKeepsOnlyThoseStates(t *testing.T) {
	reader := uuid.NewString()
	ids := viewWorld(t, reader, true, false, true)
	has := func(clause string, s State) bool { return strings.Contains(clause, ids[s]) }

	// Those states and no others.
	clause, err := FilterClause(context.Background(), project, []string{string(AtRisk), string(Breached)})
	if err != nil || !strings.HasPrefix(clause, "eq(task_uuid") || !has(clause, AtRisk) || !has(clause, Breached) || has(clause, OnTrack) || has(clause, Met) {
		t.Errorf("at risk or breached: %q (%v)", clause, err)
	}
	// No SLA: every task but those whose timers stand somewhere else.
	clause, err = FilterClause(context.Background(), project, []string{NoSLA, string(Met)})
	if err != nil || !strings.HasPrefix(clause, "NOT eq(task_uuid") || has(clause, Met) || !has(clause, OnTrack) || !has(clause, AtRisk) || !has(clause, Breached) {
		t.Errorf("none or met: %q (%v)", clause, err)
	}
	// A value that is no state matches nothing.
	if clause, err = FilterClause(context.Background(), project, []string{"late"}); err != nil || clause != taskDomain.NoTask {
		t.Errorf("an unknown state: %q (%v), want nothing", clause, err)
	}
	// SLAs off: nothing matches, not even "none".
	viewWorld(t, reader, true, false, false)
	if clause, err = FilterClause(context.Background(), project, []string{NoSLA, string(Breached)}); err != nil || clause != taskDomain.NoTask {
		t.Errorf("SLAs off: %q (%v), want nothing", clause, err)
	}
	// Someone outside the project matches nothing.
	viewWorld(t, reader, false, false, true)
	if clause, err = FilterClause(context.Background(), project, []string{NoSLA}); err != nil || clause != taskDomain.NoTask {
		t.Errorf("outside the project: %q (%v), want nothing", clause, err)
	}
	// A switch that can't be read fails closed: nothing matches.
	viewWorld(t, reader, true, false, true)
	swap(t, &onIn, func(context.Context, []string) (map[string]bool, error) {
		return map[string]bool{project.String(): true}, errors.New("policy store down")
	})
	if clause, err = FilterClause(context.Background(), project, []string{NoSLA}); err == nil || clause != "" {
		t.Errorf("an unread switch: %q (%v), want an error (the list narrows to nothing)", clause, err)
	}
}

// The SLA filter, sort and Insights slice are SLA state: a guest, even with
// a member's place, is refused them (a 403), never shown none.
func TestTheSLAViewsRefuseAGuest(t *testing.T) {
	guest := uuid.NewString()
	viewWorld(t, guest, true, true, true)
	var refused *Refused
	if _, err := FilterClause(context.Background(), project, []string{string(Breached)}); !errors.As(err, &refused) {
		t.Errorf("a guest's SLA filter: %v, want refused", err)
	}
	if _, _, err := StatesIn(context.Background(), project); !errors.As(err, &refused) {
		t.Errorf("a guest's SLA sort: %v, want refused", err)
	}
	if _, err := ReadsIn(context.Background(), []uuid.UUID{project}); !errors.As(err, &refused) {
		t.Errorf("a guest's SLA slice: %v, want refused", err)
	}
	// A member reads the states where the guest was refused.
	member := uuid.NewString()
	ids := viewWorld(t, member, true, false, true)
	states, reads, err := StatesIn(context.Background(), project)
	if err != nil || !reads || states[ids[Breached]].State != Breached || states[ids[Met]].State != Met || len(states) != 4 {
		t.Errorf("a member's states: %v %v (%v)", states, reads, err)
	}
}
