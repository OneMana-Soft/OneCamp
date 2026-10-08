//go:build integration

package business

// Goals against a real Postgres 12 with every migration and a real Dgraph
// with the production schema: progress from projects the reader is and isn't
// in, sub-goals, a number moved by check-ins, closing and reopening, the tree
// rules under concurrent edits, and who may change what.
// Run: go test -tags=integration ./business/Goal/ -run TestGoals -v

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	goalModel "github.com/akashc777/OneCamp/models/postgres/Goal"
	updateModel "github.com/akashc777/OneCamp/models/postgres/ProjectUpdate"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestGoals(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	live := "0001-01-01T00:00:00Z"
	me, other, bot := uuid.New(), uuid.New(), uuid.New()
	mine, theirs := uuid.New(), uuid.New()
	task := func(status string) map[string]any {
		return map[string]any{"dgraph.type": "Task", "task_uuid": uuid.NewString(), "task_status": status, "task_deleted_at": live}
	}
	uids := dg.Mutate(t, map[string]any{
		"uid": "_:me", "dgraph.type": "User", "user_uuid": me.String(), "user_full_name": "Maya Chen", "user_name": "maya",
		"user_projects": []map[string]any{
			{"uid": "_:mine", "dgraph.type": "Project", "project_uuid": mine.String(), "project_name": "Q4 launch", "project_deleted_at": live,
				"project_members": []map[string]any{{"uid": "_:me"}},
				// 3 of 4 done; the canceled one counts for neither side.
				"project_tasks": []map[string]any{task("done"), task("done"), task("done"), task("todo"), task("canceled")}},
		},
	})
	dg.Mutate(t, map[string]any{
		"uid": "_:other", "dgraph.type": "User", "user_uuid": other.String(), "user_full_name": "Jonas Weber",
		"user_projects": []map[string]any{
			{"uid": "_:theirs", "dgraph.type": "Project", "project_uuid": theirs.String(), "project_name": "Secret", "project_deleted_at": live,
				"project_members": []map[string]any{{"uid": "_:other"}},
				// 1 of 4 done.
				"project_tasks": []map[string]any{task("done"), task("todo"), task("todo"), task("inProgress")}},
		},
	})
	dg.Mutate(t, map[string]any{"uid": "_:bot", "dgraph.type": "User", "user_uuid": bot.String(), "user_full_name": "Robot", "is_bot": true})

	r := Reader{UUID: me, DgraphUID: uids["me"]}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	in := func(title, measure string) Input {
		return Input{Title: title, OwnerUUID: me.String(), DueDate: "2026-12-31", Measure: measure}
	}
	isRefusal := func(err error, want string) bool {
		var ge *GoalError
		return errors.As(err, &ge) && strings.Contains(ge.Error(), want)
	}

	// Projects: only ones the reader is in can be added.
	launch := in("Launch the Business tier", goalModel.MeasureProjects)
	launch.ProjectUUIDs = []string{theirs.String()}
	if _, err := Create(ctx, r, launch, now); !isRefusal(err, "only add projects you're in") {
		t.Fatalf("a project the reader isn't in can't be added: %v", err)
	}
	launch.ProjectUUIDs = []string{mine.String()}
	g1, err := Create(ctx, r, launch, now)
	if err != nil {
		t.Fatal(err)
	}
	if g1.Progress == nil || *g1.Progress != 0.75 || g1.Projects != 1 || !g1.CanEdit || g1.Owner.FullName != "Maya Chen" {
		t.Fatalf("progress from the project's tasks done, owner named: %+v", g1)
	}
	// Someone in the other project links it too: it counts, unnamed to this reader.
	if _, err := goalModel.LinkProject(uuid.MustParse(g1.Id), theirs, other, MaxProjects); err != nil {
		t.Fatal(err)
	}
	d, err := Get(ctx, r, uuid.MustParse(g1.Id), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.ProjectList) != 1 || d.ProjectList[0].Name != "Q4 launch" || d.HiddenProjects != 1 || *d.Progress != 0.5 {
		t.Fatalf("the reader sees their project, counts the other, and progress averages both (0.75, 0.25): %+v", d)
	}

	// Owners are people in the workspace.
	bad := in("Robot goal", goalModel.MeasureProjects)
	bad.OwnerUUID = bot.String()
	if _, err := Create(ctx, r, bad, now); !isRefusal(err, "Choose someone in the workspace") {
		t.Fatalf("a bot can't own a goal: %v", err)
	}
	bad.OwnerUUID = uuid.NewString()
	if _, err := Create(ctx, r, bad, now); !isRefusal(err, "Choose someone in the workspace") {
		t.Fatalf("nobody can't own a goal: %v", err)
	}

	// A number goal under a sub-goals parent.
	top, err := Create(ctx, r, in("Grow revenue", goalModel.MeasureSubgoals), now)
	if err != nil {
		t.Fatal(err)
	}
	teams := in("Reach 500 paying teams", goalModel.MeasureNumber)
	teams.StartValue, teams.TargetValue, teams.CurrentValue, teams.Unit, teams.ParentID = f(320), f(500), f(320), "teams", top.Id
	g2, err := Create(ctx, r, teams, now)
	if err != nil {
		t.Fatal(err)
	}
	launch.ParentID, launch.ProjectUUIDs = top.Id, nil
	if _, err := Edit(ctx, r, uuid.MustParse(g1.Id), launch, now); err != nil {
		t.Fatal(err)
	}

	// A check-in moves the number and records the progress it was posted at.
	user := &userModels.UserInfo{}
	user.UserPostgresInfo.Id = me
	posted, err := PostCheckIn(ctx, user, r, uuid.MustParse(g2.Id), CheckInInput{Health: updateModel.AtRisk, Value: f(410)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if posted.CheckIn.Progress == nil || *posted.CheckIn.Progress != 0.5 || posted.CheckIn.AuthorName != "Maya Chen" {
		t.Fatalf("the check-in keeps the progress it moved to: %+v", posted.CheckIn)
	}
	list, err := List(ctx, r, now)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Summary{}
	for _, s := range list {
		byID[s.Id] = s
	}
	if s := byID[g2.Id]; *s.CurrentValue != 410 || s.Health != updateModel.AtRisk || s.CheckedInAt == nil {
		t.Fatalf("the number moved and the health is the check-in's: %+v", s)
	}
	if s := byID[top.Id]; s.Progress == nil || *s.Progress != 0.5 || s.Subgoals != 2 {
		t.Fatalf("the parent averages its sub-goals (0.5 and 0.5): %+v", s)
	}

	// The draft reads the projects and the last check-in.
	draft, err := MakeDraft(ctx, r, uuid.MustParse(g1.Id), now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(draft.Text, "3 of 4 tasks done across 1 project") || !strings.Contains(draft.Text, "1 more you're not in counts too.") || strings.Contains(draft.Text, "Q4 launch") {
		t.Fatalf("draft:\n%s", draft.Text)
	}

	// An edit that doesn't give the number keeps where check-ins moved it.
	keep := in("Reach 500 paying teams", goalModel.MeasureNumber)
	keep.StartValue, keep.TargetValue, keep.Unit, keep.ParentID = f(320), f(500), "teams", top.Id
	if s, err := Edit(ctx, r, uuid.MustParse(g2.Id), keep, now); err != nil || s.CurrentValue == nil || *s.CurrentValue != 410 {
		t.Fatalf("the edit kept the number at 410: %+v %v", s, err)
	}

	// Someone who can't change a goal can't put a goal under it, nor take one out.
	outsider := Reader{UUID: other, DgraphUID: "0x1"}
	under := in("Mine, under theirs", goalModel.MeasureNumber)
	under.OwnerUUID, under.StartValue, under.TargetValue, under.CurrentValue, under.ParentID = other.String(), f(0), f(1), f(1), top.Id
	if _, err := Create(ctx, outsider, under, now); !isRefusal(err, "can put a goal under it") {
		t.Fatalf("a stranger attached a goal to someone else's: %v", err)
	}
	under.ParentID = ""
	theirGoal, err := Create(ctx, outsider, under, now)
	if err != nil {
		t.Fatal(err)
	}
	under.ParentID = top.Id
	if _, err := Edit(ctx, outsider, uuid.MustParse(theirGoal.Id), under, now); !isRefusal(err, "can put a goal under it") {
		t.Fatalf("a stranger moved their goal under someone else's: %v", err)
	}

	// Of two closings at once, one lands and the other is told the goal closed.
	race, _ := Create(ctx, r, in("Race", goalModel.MeasureProjects), now)
	var closers sync.WaitGroup
	closeErrs := make([]error, 2)
	for i, ending := range []string{goalModel.StatusAchieved, goalModel.StatusDropped} {
		closers.Add(1)
		go func(i int, ending string) {
			defer closers.Done()
			_, closeErrs[i] = PostCheckIn(ctx, user, r, uuid.MustParse(race.Id), CheckInInput{Health: ending}, now)
		}(i, ending)
	}
	closers.Wait()
	if (closeErrs[0] == nil) == (closeErrs[1] == nil) || !(isRefusal(closeErrs[0], "closed") || isRefusal(closeErrs[1], "closed")) {
		t.Fatalf("exactly one closing lands: %v / %v", closeErrs[0], closeErrs[1])
	}
	// Reopened, its last check-in (the closing one) is no longer its health.
	if err := Reopen(r, uuid.MustParse(race.Id)); err != nil {
		t.Fatal(err)
	}
	if s, _ := summaryOf(ctx, r, uuid.MustParse(race.Id), now); s.Health != "" || s.CheckedInAt != nil {
		t.Fatalf("a reopened goal waits for a new check-in: %q", s.Health)
	}

	// Closing keeps the progress; a closed goal takes no check-in until reopened.
	if _, err := PostCheckIn(ctx, user, r, uuid.MustParse(g2.Id), CheckInInput{Health: goalModel.StatusAchieved, Value: f(520)}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := PostCheckIn(ctx, user, r, uuid.MustParse(g2.Id), CheckInInput{Health: updateModel.OnTrack, Body: "more"}, now); !isRefusal(err, "Reopen it") {
		t.Fatalf("a closed goal takes no check-in: %v", err)
	}
	closed, _ := goalModel.Get(uuid.MustParse(g2.Id))
	if closed.Status != goalModel.StatusAchieved || closed.FinalProgress == nil || *closed.FinalProgress != 1 || closed.ClosedAt == nil {
		t.Fatalf("closed as achieved at 100%%: %+v", closed)
	}
	if err := Reopen(r, closed.Id); err != nil {
		t.Fatal(err)
	}
	if again, _ := goalModel.Get(closed.Id); again.Status != goalModel.StatusOpen || again.FinalProgress != nil || again.ClosedAt != nil {
		t.Fatalf("reopened: %+v", again)
	}

	// The tree: no loops, even when two people move goals under each other at once.
	topID := uuid.MustParse(top.Id)
	loop := in("Grow revenue", goalModel.MeasureSubgoals)
	loop.ParentID = g1.Id
	if _, err := Edit(ctx, r, topID, loop, now); !isRefusal(err, "itself or one of its own") {
		t.Fatalf("a goal can't go under its own sub-goal: %v", err)
	}
	x, _ := Create(ctx, r, in("X", goalModel.MeasureSubgoals), now)
	y, _ := Create(ctx, r, in("Y", goalModel.MeasureSubgoals), now)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, pair := range [][2]*Summary{{x, y}, {y, x}} {
		wg.Add(1)
		go func(i int, child, parent *Summary) {
			defer wg.Done()
			edit := in(child.Title, goalModel.MeasureSubgoals)
			edit.ParentID = parent.Id
			_, errs[i] = Edit(ctx, r, uuid.MustParse(child.Id), edit, now)
		}(i, pair[0], pair[1])
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("exactly one of two crossing moves may land: %v / %v", errs[0], errs[1])
	}

	// Only the owner, the creator or a workspace admin changes a goal.
	stranger := Reader{UUID: other, DgraphUID: "0x1"}
	if _, err := Edit(ctx, stranger, topID, in("Mine now", goalModel.MeasureSubgoals), now); !errors.Is(err, ErrNotYours) {
		t.Fatalf("a stranger can't edit: %v", err)
	}
	stranger.IsAdmin = true
	if _, err := Edit(ctx, stranger, topID, in("Grow revenue 40%", goalModel.MeasureSubgoals), now); err != nil {
		t.Fatalf("a workspace admin can: %v", err)
	}

	// The project's page lists the open goals it serves.
	for_, err := ForProject(ctx, r, mine, now)
	if err != nil || len(for_) != 1 || for_[0].Id != g1.Id {
		t.Fatalf("the project serves one goal: %+v %v", for_, err)
	}

	// An edit that keeps the parent doesn't write it: a deletion of the parent
	// that lands first still moves the goal up.
	mid, _ := Create(ctx, r, in("Middle", goalModel.MeasureSubgoals), now)
	leafIn := in("Leaf", goalModel.MeasureSubgoals)
	leafIn.ParentID = mid.Id
	leaf, err := Create(ctx, r, leafIn, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := Delete(r, uuid.MustParse(mid.Id)); err != nil {
		t.Fatal(err)
	}
	leafIn.Title = "Leaf, renamed" // still carrying the parent it had when the editor opened
	if _, err := Edit(ctx, r, uuid.MustParse(leaf.Id), leafIn, now); !isRefusal(err, "no longer exists") {
		// The editor's copy names a deleted parent as a change it isn't; either it is
		// refused, or it saves without writing the parent back.
		if err != nil {
			t.Fatalf("editing the leaf: %v", err)
		}
	}
	if got, _ := goalModel.Get(uuid.MustParse(leaf.Id)); got.ParentId != nil {
		t.Fatalf("the leaf points at its deleted parent again: %v", got.ParentId)
	}

	// Deleting a parent moves its sub-goals up, never leaving them pointing at nothing.
	if err := Delete(r, topID); err != nil {
		t.Fatal(err)
	}
	if child, _ := goalModel.Get(uuid.MustParse(g1.Id)); child.ParentId != nil {
		t.Fatalf("its sub-goal moved to the top: %+v", child.ParentId)
	}

	// The cap on a goal's projects holds.
	if _, err := goalModel.LinkProject(uuid.MustParse(g1.Id), uuid.New(), me, 2); !errors.Is(err, goalModel.ErrTooManyProjects) {
		t.Fatalf("a third project over a cap of two is refused: %v", err)
	}
	if added, err := goalModel.LinkProject(uuid.MustParse(g1.Id), mine, me, 2); err != nil || added {
		t.Fatalf("linking one already there is a no-op, even at the cap: %v %v", added, err)
	}
}
