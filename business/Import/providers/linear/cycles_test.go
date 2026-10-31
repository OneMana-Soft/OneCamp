package linear

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

func at(day int) time.Time { return time.Date(2026, 9, day, 9, 0, 0, 0, time.UTC) }

// L-2: a project gets the team cycles its issues are in, named as Linear
// shows them ("Cycle 12" when unnamed), and a closed one keeps how many of
// the project's issues it finished. Its tasks say their cycle, and the
// project carries its team's cycle settings. Before, cycles were dropped.
func TestAProjectGetsTheCyclesItsIssuesAreIn(t *testing.T) {
	closed := at(15)
	p := New()
	job := &importModels.Job{Id: uuid.New()}
	schedule := &importProvider.SourceCycleSchedule{Weeks: 2, Upcoming: 3, RollOver: true, Anchor: at(15)}
	p.snapshotCache[job.Id] = &workspaceSnapshot{
		Projects: []linearProject{{ID: "proj", Name: "Launch", TeamID: "eng"}},
		Issues: []linearIssue{
			{ID: "a", TeamID: "eng", ProjectID: "proj", CycleID: "c12", CompletedAt: &closed},
			{ID: "b", TeamID: "eng", ProjectID: "proj", CycleID: "c12"},
			{ID: "a1", TeamID: "eng", ProjectID: "proj", CycleID: "c14", ParentID: "a"},
			{ID: "c", TeamID: "eng", ProjectID: "proj", CycleID: "c13", CompletedAt: &closed},
			{ID: "d", TeamID: "eng", ProjectID: "proj"},
			{ID: "elsewhere", TeamID: "eng", ProjectID: "other", CycleID: "c11"},
		},
		cycleByID: map[string]linearCycle{
			"c11": {ID: "c11", Number: 11, StartsAt: at(1), EndsAt: at(8), CompletedAt: &closed},
			"c12": {ID: "c12", Number: 12, StartsAt: at(1), EndsAt: at(15), CompletedAt: &closed},
			"c13": {ID: "c13", Number: 13, Name: "Polish", StartsAt: at(15), EndsAt: at(29)},
			"c14": {ID: "c14", Number: 14, StartsAt: at(29), EndsAt: at(30)},
		},
		schedules: map[string]*importProvider.SourceCycleSchedule{"eng": schedule},
	}

	projects, errs := p.IterProjects(context.Background(), job, nil)
	var got []importProvider.SourceCycle
	for sp := range projects {
		got = sp.Cycles
		if sp.CycleSchedule != schedule {
			t.Errorf("the project carries its team's cycle settings: %+v", sp.CycleSchedule)
		}
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("the two cycles its tasks are in (a sub-issue's comes inside its parent's task), got %+v", got)
	}
	if got[0].SourceID != "c12" || got[0].Name != "Cycle 12" || got[0].CompletedAt == nil || got[0].Done != 1 || !got[0].EndsAt.Equal(at(15)) {
		t.Errorf("the closed cycle, named as Linear shows it, with the one issue it finished: %+v", got[0])
	}
	if got[1].SourceID != "c13" || got[1].Name != "Polish" || got[1].CompletedAt != nil || got[1].Done != 0 {
		t.Errorf("the running cycle, by its own name, with nothing counted finished: %+v", got[1])
	}

	tasks, errs := p.IterTasksOfProject(context.Background(), job, nil, "proj")
	in := map[string]string{}
	for st := range tasks {
		in[st.SourceID] = st.CycleSourceID
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if in["a"] != "c12" || in["c"] != "c13" || in["d"] != "" {
		t.Fatalf("each task says its cycle: %v", in)
	}
}

// A team's settings become the project's schedule: its cycles fall on the
// weeks of the cycle running now (else the last that started, else the first
// to come), and Linear moves unfinished issues on. A team with cycles off has
// none.
func TestATeamsCycleSettingsBecomeASchedule(t *testing.T) {
	team := linearTeam{ID: "eng", CyclesEnabled: true, CycleWeeks: 2, CooldownWeeks: 1, Upcoming: 4}
	cycles := []linearCycle{{StartsAt: at(1)}, {StartsAt: at(22)}, {StartsAt: at(8)}}

	s := cycleSchedule(team, cycles, at(10))
	if s == nil || !s.Anchor.Equal(at(8)) || s.Weeks != 2 || s.CooldownWeeks != 1 || s.Upcoming != 4 || !s.RollOver {
		t.Fatalf("the running cycle's weeks and the team's settings: %+v", s)
	}
	if s := cycleSchedule(team, cycles, at(1).Add(-time.Hour)); s == nil || !s.Anchor.Equal(at(1)) {
		t.Fatalf("with every cycle still to come, the first one's weeks: %+v", s)
	}
	team.CyclesEnabled = false
	if s := cycleSchedule(team, cycles, at(10)); s != nil {
		t.Fatalf("cycles off, no schedule: %+v", s)
	}
}

// Every team's cycles are read in one list, page by page, closed ones and all.
func TestEveryTeamsCyclesAreReadEveryPage(t *testing.T) {
	var hits int32
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := readBody(r)
		if !strings.Contains(body, "cycles(") || !strings.Contains(body, "includeArchived: true") {
			t.Errorf("every cycle, archived too: %q", body)
		}
		if atomic.AddInt32(&hits, 1) == 1 {
			_, _ = w.Write([]byte(`{"data": {"cycles": {
				"nodes": [{"id": "c1", "number": 1, "name": null, "startsAt": "2026-09-01T00:00:00Z", "endsAt": "2026-09-15T00:00:00Z", "completedAt": "2026-09-15T00:00:00Z", "team": {"id": "eng"}}],
				"pageInfo": {"hasNextPage": true, "endCursor": "p1"}}}}`))
			return
		}
		if !strings.Contains(body, `"cursor":"p1"`) {
			t.Errorf("page two asks after page one: %q", body)
		}
		_, _ = w.Write([]byte(`{"data": {"cycles": {
			"nodes": [{"id": "c2", "number": 2, "name": "Polish", "startsAt": "2026-09-15T00:00:00Z", "endsAt": "2026-09-29T00:00:00Z", "completedAt": null, "team": {"id": "mob"}}],
			"pageInfo": {"hasNextPage": false, "endCursor": ""}}}}`))
	})
	got, err := New().fetchCycles(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].name() != "Cycle 1" || got[0].CompletedAt == nil || got[0].Team.ID != "eng" ||
		got[1].name() != "Polish" || got[1].CompletedAt != nil || got[1].Team.ID != "mob" {
		t.Fatalf("both pages' cycles, each with its team: %+v", got)
	}
}
