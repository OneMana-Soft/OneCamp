package business

import (
	"errors"
	"strings"
	"testing"
	"time"

	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	goalModel "github.com/akashc777/OneCamp/models/postgres/Goal"
	updateModel "github.com/akashc777/OneCamp/models/postgres/ProjectUpdate"
	"github.com/google/uuid"
)

func f(v float64) *float64 { return &v }

func validInput() Input {
	return Input{Title: "  Reach   500 paying teams ", OwnerUUID: uuid.NewString(), DueDate: "2026-12-31", Measure: goalModel.MeasureProjects}
}

func refusal(t *testing.T, err error) string {
	t.Helper()
	var ge *GoalError
	if !errors.As(err, &ge) {
		t.Fatalf("want a GoalError, got %v", err)
	}
	return ge.Error()
}

func TestCheck(t *testing.T) {
	g, projects, err := Check(validInput())
	if err != nil {
		t.Fatal(err)
	}
	if g.Title != "Reach 500 paying teams" {
		t.Errorf("title is tidied, got %q", g.Title)
	}
	if g.StartValue != nil || len(projects) != 0 {
		t.Errorf("a projects goal keeps no number and links nothing unasked")
	}

	cases := map[string]func(*Input){
		"Give the goal a title":           func(in *Input) { in.Title = "   " },
		"Choose who owns the goal":        func(in *Input) { in.OwnerUUID = "" },
		"Choose the date the goal is due": func(in *Input) { in.DueDate = "31/12/2026" },
		"between 2000 and 2100":           func(in *Input) { in.DueDate = "2200-01-01" },
		"can't start after it's due":      func(in *Input) { in.StartDate = "2027-01-01" },
		"Choose how the goal measures":    func(in *Input) { in.Measure = "vibes" },
		"where it starts, its target":     func(in *Input) { in.Measure = goalModel.MeasureNumber; in.StartValue = f(1) },
		"has to differ": func(in *Input) {
			in.Measure = goalModel.MeasureNumber
			in.StartValue, in.TargetValue, in.CurrentValue = f(5), f(5), f(5)
		},
		"isn't one OneCamp knows":          func(in *Input) { in.ParentID = "nope" },
		"One of those projects isn't":      func(in *Input) { in.ProjectUUIDs = []string{"x"} },
		"Keep a goal's title under 200":    func(in *Input) { in.Title = strings.Repeat("a", 201) },
		"Keep the description under 4,000": func(in *Input) { in.Description = strings.Repeat("a", 4001) },
		"Keep the unit under 24": func(in *Input) {
			in.Measure = goalModel.MeasureNumber
			in.StartValue, in.TargetValue, in.CurrentValue = f(0), f(1), f(0)
			in.Unit = strings.Repeat("u", 25)
		},
		"A goal can have up to 20 projects": func(in *Input) {
			for range 21 {
				in.ProjectUUIDs = append(in.ProjectUUIDs, uuid.NewString())
			}
		},
	}
	for want, change := range cases {
		in := validInput()
		change(&in)
		if _, _, err := Check(in); !strings.Contains(refusal(t, err), want) {
			t.Errorf("%s: got %q", want, err)
		}
	}

	in := validInput()
	in.Measure, in.StartValue, in.TargetValue, in.CurrentValue, in.Unit = goalModel.MeasureNumber, f(320), f(500), f(410), " teams "
	same := uuid.NewString()
	in.ProjectUUIDs = []string{same, same}
	g, projects, err = Check(in)
	if err != nil || g.Unit != "teams" || *g.CurrentValue != 410 || len(projects) != 1 {
		t.Errorf("a number goal keeps its values and unit, projects once each: %+v %v %v", g, projects, err)
	}
}

func TestNumberProgress(t *testing.T) {
	for _, c := range []struct{ start, target, current, want float64 }{
		{320, 500, 410, 0.5},
		{320, 500, 600, 1}, // past the target is done, not more than done
		{320, 500, 100, 0}, // below the start is not negative
		{6, 2, 3, 0.75},    // a target below the start counts downwards
		{5, 5, 5, 0},       // no distance to go is guarded
	} {
		if got := NumberProgress(c.start, c.target, c.current); got != c.want {
			t.Errorf("NumberProgress(%v, %v, %v) = %v, want %v", c.start, c.target, c.current, got, c.want)
		}
	}
	if TaskProgress(0, 0) != nil || *TaskProgress(3, 1) != 0.25 {
		t.Errorf("task progress: none for no tasks, done over all")
	}
}

func TestExpectedAndPace(t *testing.T) {
	loc := time.FixedZone("IST", 5*3600+1800)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC) // ten days, to the end of the 10th
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, loc)
	if got := Expected(start, due, now); got != 0.5 {
		t.Errorf("half the days gone in the reader's own calendar, got %v", got)
	}
	if Expected(start, due, start.AddDate(0, 0, -3).In(loc)) != 0 || Expected(start, due, due.AddDate(0, 1, 0)) != 1 {
		t.Errorf("expected is 0 before the start and 1 after the due date")
	}
	for _, c := range []struct {
		progress *float64
		want     string
	}{{f(0.45), updateModel.OnTrack}, {f(0.30), updateModel.AtRisk}, {f(0.2), updateModel.OffTrack}, {nil, ""}} {
		if got := PaceHealth(c.progress, 0.5); got != c.want {
			t.Errorf("PaceHealth(%v) = %q, want %q", c.progress, got, c.want)
		}
	}
}

func TestAmount(t *testing.T) {
	for _, c := range []struct {
		v    float64
		unit string
		want string
	}{
		{410, "teams", "410 teams"},
		{250000, "$", "$250,000"},
		{1500000, "₹", "₹1,500,000"},
		{12.5, "%", "12.5%"},
		{3.456, "hours", "3.46 hours"},
		{-1200, "", "-1,200"},
		{0.25, "", "0.25"},
	} {
		if got := Amount(c.v, c.unit); got != c.want {
			t.Errorf("Amount(%v, %q) = %q, want %q", c.v, c.unit, got, c.want)
		}
	}
}

func node(id uuid.UUID, parent *uuid.UUID) goalModel.Node {
	return goalModel.Node{Id: id, ParentId: parent}
}

func TestCheckParent(t *testing.T) {
	a, b, c, d, e := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	// a > b > c, and d alone.
	nodes := []goalModel.Node{node(a, nil), node(b, &a), node(c, &b), node(d, nil)}
	if err := CheckParent(nodes, uuid.Nil, c); err != nil {
		t.Errorf("a new goal may be the fourth level: %v", err)
	}
	nodes = append(nodes, node(e, &c))
	if err := CheckParent(nodes, uuid.Nil, e); !strings.Contains(refusal(t, err), "4 levels") {
		t.Errorf("a fifth level is refused: %v", err)
	}
	if err := CheckParent(nodes, a, c); !strings.Contains(refusal(t, err), "itself or one of its own") {
		t.Errorf("a goal can't go under its own sub-goal: %v", err)
	}
	if err := CheckParent(nodes, a, a); err == nil {
		t.Errorf("a goal can't be its own parent")
	}
	if err := CheckParent(nodes, d, b); err != nil {
		t.Errorf("a leaf may move under the second level: %v", err)
	}
	if err := CheckParent(nodes, b, d); err != nil {
		t.Errorf("a three-level branch may move under a top goal, making four: %v", err)
	}
	if err := CheckParent(nodes, a, d); !strings.Contains(refusal(t, err), "4 levels") {
		t.Errorf("a four-level branch under another goal would make five: %v", err)
	}
	if err := CheckParent(nodes, d, uuid.New()); !strings.Contains(refusal(t, err), "no longer exists") {
		t.Errorf("a missing parent is refused: %v", err)
	}
}

// boardOf builds a board by hand, as load would read it.
func boardOf(now time.Time, goals ...*goalModel.Goal) *board {
	b := &board{now: now, goals: map[uuid.UUID]*goalModel.Goal{}, children: map[uuid.UUID][]uuid.UUID{},
		links: map[uuid.UUID][]uuid.UUID{}, projects: map[string]projectDomain.ProjectProgress{},
		latest: map[uuid.UUID]goalModel.CheckIn{}, progress: map[uuid.UUID]*float64{}}
	for _, g := range goals {
		b.goals[g.Id] = g
		b.order = append(b.order, g.Id)
	}
	for _, g := range goals {
		if g.ParentId != nil {
			b.children[*g.ParentId] = append(b.children[*g.ParentId], g.Id)
		}
	}
	return b
}

func goal(measure string, parent *uuid.UUID) *goalModel.Goal {
	return &goalModel.Goal{Id: uuid.New(), Title: measure, Measure: measure, ParentId: parent, Status: goalModel.StatusOpen,
		DueDate: time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
}

func TestProgress(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	top := goal(goalModel.MeasureSubgoals, nil)
	byProjects := goal(goalModel.MeasureProjects, &top.Id)
	byNumber := goal(goalModel.MeasureNumber, &top.Id)
	byNumber.StartValue, byNumber.TargetValue, byNumber.CurrentValue = f(0), f(100), f(20)
	dropped := goal(goalModel.MeasureNumber, &top.Id)
	dropped.StartValue, dropped.TargetValue, dropped.CurrentValue = f(0), f(100), f(0)
	dropped.Status, dropped.FinalProgress = goalModel.StatusDropped, f(0)
	empty := goal(goalModel.MeasureSubgoals, nil)
	b := boardOf(now, top, byProjects, byNumber, dropped, empty)

	full, half, none, gone, hidden := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	b.links[byProjects.Id] = []uuid.UUID{full, half, none, gone, hidden}
	b.projects[full.String()] = projectDomain.ProjectProgress{UUID: full.String(), Done: 4, IsMember: 1}
	b.projects[half.String()] = projectDomain.ProjectProgress{UUID: half.String(), Open: 5, Done: 5, IsMember: 1}
	b.projects[none.String()] = projectDomain.ProjectProgress{UUID: none.String(), IsMember: 1} // no tasks: no say
	archived := time.Now()
	b.projects[gone.String()] = projectDomain.ProjectProgress{UUID: gone.String(), Open: 9, DeletedAt: &archived, IsMember: 1}
	// A project the reader isn't in still counts towards the goal.
	b.projects[hidden.String()] = projectDomain.ProjectProgress{UUID: hidden.String(), Open: 1, Done: 1}

	// Each project with tasks counts the same: (1 + 0.5 + 0.5) / 3.
	if p := b.progressOf(byProjects.Id); p == nil || *p < 0.666 || *p > 0.667 {
		t.Errorf("projects progress is the average of each live project's share done, got %v", p)
	}
	// The parent averages its sub-goals, leaving out the dropped one: (2/3 + 0.2) / 2.
	if p := b.progressOf(top.Id); p == nil || *p < 0.433 || *p > 0.434 {
		t.Errorf("sub-goals progress averages the live sub-goals, got %v", p)
	}
	if b.progressOf(empty.Id) != nil {
		t.Errorf("a goal with nothing to measure has no progress, not 0")
	}

	// A closed goal keeps the progress it closed at.
	byNumber.Status, byNumber.FinalProgress = goalModel.StatusAchieved, f(1)
	b.progress = map[uuid.UUID]*float64{}
	if p := b.progressOf(byNumber.Id); p == nil || *p != 1 {
		t.Errorf("a closed goal keeps its final progress, got %v", p)
	}

	// A loop in old data ends instead of recursing for ever.
	loopA, loopB := goal(goalModel.MeasureSubgoals, nil), goal(goalModel.MeasureSubgoals, nil)
	loopA.ParentId, loopB.ParentId = &loopB.Id, &loopA.Id
	lb := boardOf(now, loopA, loopB)
	_ = lb.progressOf(loopA.Id)

	s := b.summary(byProjects, nil)
	if s.Projects != 5 || s.ParentId != top.Id.String() || s.Expected == nil || s.Owner.Uuid == "" {
		t.Errorf("the summary carries counts, parent, pace and owner: %+v", s)
	}
	lines, hiddenCount := b.lines(byProjects.Id)
	if len(lines) != 4 || hiddenCount != 1 {
		t.Errorf("the page names the projects the reader is in (archived too) and counts the rest: %d shown, %d hidden", len(lines), hiddenCount)
	}
}

func TestDraft(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	was := now.AddDate(0, 0, -7)
	updated := now.AddDate(0, 0, -2)
	fx := Facts{
		Goal: "Launch the Business tier", Measure: goalModel.MeasureProjects, Status: goalModel.StatusOpen,
		Progress: f(0.55), Previous: f(0.42), PreviousAt: &was, Expected: f(0.7),
		Due: time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
		Projects: []ProjectLine{
			{Name: "Q4 launch", Open: 8, Done: 12, Overdue: 2, Health: updateModel.OnTrack, UpdatedAt: &updated},
			{Name: "Website", Open: 7, Done: 3},
		},
		HiddenProjects: 1,
		Subgoals:       []Summary{{Title: "Reach 500 teams", Status: goalModel.StatusOpen, Progress: f(0.82), Health: updateModel.AtRisk}},
	}
	text := DraftText(fx, now)
	for _, want := range []string{
		"Progress is 55%, up from 42% at the last check-in, 7 days ago.",
		"70% of its time to Thu 31 Dec has passed.",
		"Projects: 15 of 30 tasks done across 2 projects, 2 overdue. Their last updates: 1 on track, 1 with no update yet. 1 more you're not in counts too.",
		"- Reach 500 teams: 82%, at risk.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("draft lacks %q:\n%s", want, text)
		}
	}
	// The workspace reads check-ins: a project is never named in one.
	for _, name := range []string{"Q4 launch", "Website"} {
		if strings.Contains(text, name) {
			t.Errorf("the draft names the project %q:\n%s", name, text)
		}
	}
	// 15 points behind its time: at risk.
	if h := SuggestHealth(fx); h != updateModel.AtRisk {
		t.Errorf("suggested %q, want at risk", h)
	}

	num := Facts{Goal: "Teams", Measure: goalModel.MeasureNumber, Status: goalModel.StatusOpen, Progress: f(0.5),
		Start: f(320), Target: f(500), Current: f(410), Unit: "teams", Due: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	if text := DraftText(num, now); !strings.Contains(text, "Now at 410 teams: 50% of the way from 320 teams to 500 teams.") || !strings.Contains(text, "It was due on Sun 1 Nov.") {
		t.Errorf("a number goal's draft says where the number is, and that it's past due:\n%s", text)
	}
	if text := DraftText(Facts{Measure: goalModel.MeasureProjects, Status: goalModel.StatusOpen}, now); !strings.HasPrefix(text, "No project serves this goal yet.") {
		t.Errorf("a goal without projects says so: %q", text)
	}
	// With nothing to measure, the worst report of its projects and sub-goals.
	quiet := Facts{Projects: []ProjectLine{{Health: updateModel.OffTrack}}, Subgoals: []Summary{{Status: goalModel.StatusOpen, Health: updateModel.AtRisk}}}
	if h := SuggestHealth(quiet); h != updateModel.OffTrack {
		t.Errorf("suggested %q, want off track", h)
	}
}

func TestCheckCheckIn(t *testing.T) {
	g := goal(goalModel.MeasureProjects, nil)
	if _, err := CheckCheckIn(g, CheckInInput{Health: "great"}); err == nil {
		t.Errorf("an unknown health is refused")
	}
	if _, err := CheckCheckIn(g, CheckInInput{Health: updateModel.OnTrack}); !strings.Contains(refusal(t, err), "Write a few words") {
		t.Errorf("an open check-in needs a note: %v", err)
	}
	if _, err := CheckCheckIn(g, CheckInInput{Health: goalModel.StatusAchieved}); err != nil {
		t.Errorf("closing needs no note: %v", err)
	}
	if _, err := CheckCheckIn(g, CheckInInput{Health: updateModel.OnTrack, Value: f(3)}); !strings.Contains(refusal(t, err), "Only a goal measured by a number") {
		t.Errorf("a value needs a number goal: %v", err)
	}
	n := goal(goalModel.MeasureNumber, nil)
	if in, err := CheckCheckIn(n, CheckInInput{Health: updateModel.OnTrack, Value: f(3), Body: "  "}); err != nil || in.Body != "" {
		t.Errorf("a number goal's check-in may be only the value: %v", err)
	}
	g.Status = goalModel.StatusMissed
	if _, err := CheckCheckIn(g, CheckInInput{Health: updateModel.OnTrack, Body: "hi"}); !strings.Contains(refusal(t, err), "Reopen it") {
		t.Errorf("a closed goal takes no check-in: %v", err)
	}
}
