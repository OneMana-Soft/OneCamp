package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	triageModel "github.com/akashc777/OneCamp/models/postgres/Triage"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// rotaWorld answers the rota's seams: the rota a project keeps, who is in
// the project, what was kept, and what arriving work told the engine.
type rotaWorld struct {
	*world
	rotas   map[string]Rota
	members map[string]bool
	kept    []Rota
	reads   int
	arrived []map[string]interface{}
}

// standing is the caller's standing in project as the route checked it
// (projectaccess.RequireStanding) and hands it down: the world's role.
func (r *rotaWorld) standing(project uuid.UUID) workAccess.Standing {
	return workAccess.Standing{ProjectUUID: project.String(), Role: r.role}
}

func newRotaWorld(t *testing.T) *rotaWorld {
	t.Helper()
	r := &rotaWorld{world: newWorld(t), rotas: map[string]Rota{}, members: map[string]bool{}}
	read0, write0, members0, arrived0 := readRota, writeRota, membersIn, arrivedEvent
	t.Cleanup(func() { readRota, writeRota, membersIn, arrivedEvent = read0, write0, members0, arrived0 })
	readRota = func(_ context.Context, projectID string) (Rota, error) {
		r.reads++
		return r.rotas[projectID], nil
	}
	writeRota = func(_ context.Context, projectID string, rota Rota, _ policyBusiness.Changer) error {
		r.kept = append(r.kept, rota)
		r.rotas[projectID] = rota
		return nil
	}
	membersIn = func(_ context.Context, _ string, ids []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, id := range ids {
			_, out[id] = r.members[id]
		}
		return out, nil
	}
	arrivedEvent = func(_ context.Context, eventType string, data map[string]interface{}) {
		data["event"] = eventType
		r.arrived = append(r.arrived, data)
	}
	return r
}

// Who is on duty is the rota's turns counted from its start, a turn a
// number of weeks long, round and round; before it starts, and with no one
// on it, nobody is. Turns change at 00:00 UTC.
func TestOnDutyTurnsByWeeks(t *testing.T) {
	ann, ben, cy := uuid.NewString(), uuid.NewString(), uuid.NewString()
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	weekly := Rota{People: []string{ann, ben, cy}, Weeks: 1, Start: "2026-11-02"}
	fortnightly := Rota{People: []string{ann, ben}, Weeks: 2, Start: "2026-11-02"}
	for _, c := range []struct {
		name string
		rota Rota
		now  time.Time
		want string
	}{
		{"before it starts, nobody", weekly, at("2026-11-01T23:59:59Z"), ""},
		{"the first turn", weekly, at("2026-11-02T00:00:00Z"), ann},
		{"the first turn's last second", weekly, at("2026-11-08T23:59:59Z"), ann},
		{"the second turn", weekly, at("2026-11-09T00:00:00Z"), ben},
		{"the third turn", weekly, at("2026-11-16T12:00:00Z"), cy},
		{"round again", weekly, at("2026-11-23T00:00:00Z"), ann},
		{"a year on: week 52, 52 % 3 is 1", weekly, at("2027-11-01T00:00:00Z"), ben},
		{"two weeks a turn, still the first", fortnightly, at("2026-11-15T23:00:00Z"), ann},
		{"two weeks a turn, the next", fortnightly, at("2026-11-16T00:00:00Z"), ben},
		{"one person is always on", Rota{People: []string{cy}, Weeks: 3, Start: "2026-11-02"}, at("2030-06-01T00:00:00Z"), cy},
		{"no one on it", Rota{Weeks: 1, Start: "2026-11-02"}, at("2026-11-09T00:00:00Z"), ""},
		{"no start", Rota{People: []string{ann}, Weeks: 1}, at("2026-11-09T00:00:00Z"), ""},
		{"no length", Rota{People: []string{ann}, Start: "2026-11-02"}, at("2026-11-09T00:00:00Z"), ""},
		{"a time in another zone counts in UTC", weekly, time.Date(2026, 11, 9, 1, 0, 0, 0, time.FixedZone("IST", 19800)), ann},
	} {
		if got, _ := onDuty(c.rota, c.now); got != c.want {
			t.Errorf("%s: on duty %q, want %q", c.name, got, c.want)
		}
	}
}

// The rota is the project admins' to set: a member, a viewer and someone
// outside the project are refused, and so is an admin through an API token
// (tokens act as members, and the rota is set in the app). Nothing is kept.
func TestRotaPutRefusesMembersGuestsAndTokens(t *testing.T) {
	w := newRotaWorld(t)
	project, ann := uuid.New(), uuid.NewString()
	w.members[ann] = false
	rota := Rota{People: []string{ann}, Weeks: 1, Start: "2026-10-19"} // the world's clock is 23 Oct
	for _, role := range []workAccess.Role{workAccess.RoleNone, workAccess.RoleViewer, workAccess.RoleMember} {
		w.role = role
		if _, err := SetRota(context.Background(), member(), w.standing(project), project, rota); err == nil {
			t.Errorf("%q set the rota", role)
		}
	}
	w.role = workAccess.RoleAdmin
	if _, err := SetRota(helpers.WithActsThroughToken(context.Background()), member(), w.standing(project), project, rota); !errors.Is(err, ErrRotaThroughToken) {
		t.Errorf("an admin's token: %v", err)
	}
	if len(w.kept) != 0 {
		t.Fatalf("a refused rota was kept: %+v", w.kept)
	}
	got, err := SetRota(context.Background(), member(), w.standing(project), project, rota)
	if err != nil || len(w.kept) != 1 || got.OnDuty != ann {
		t.Fatalf("an admin sets it: %+v, %v, kept %d", got, err, len(w.kept))
	}
}

// Whoever the queue admits reads the rota and who is on duty now; someone
// outside the project finds nothing.
func TestRotaGetIsForWhoeverTheQueueAdmits(t *testing.T) {
	w := newRotaWorld(t)
	project, ann, ben := uuid.New(), uuid.NewString(), uuid.NewString()
	w.rotas[project.String()] = Rota{People: []string{ann, ben}, Weeks: 1, Start: "2026-10-12"} // 23 Oct is in the second week
	w.role = workAccess.RoleViewer
	got, err := ReadRota(context.Background(), w.standing(project), project)
	if err != nil || got.OnDuty != ben || len(got.People) != 2 || got.Weeks != 1 {
		t.Fatalf("a viewer reads it: %+v, %v", got, err)
	}
	w.role = workAccess.RoleNone
	if _, err := ReadRota(context.Background(), w.standing(project), project); !errors.Is(err, taskBusiness.ErrTaskNotVisible) {
		t.Errorf("an outsider: %v", err)
	}
	w.role = workAccess.RoleMember
	none := uuid.New()
	if empty, err := ReadRota(context.Background(), w.standing(none), none); err != nil || empty.OnDuty != "" || empty.People == nil {
		t.Errorf("a project without a rota reads as an empty one: %+v, %v", empty, err)
	}
}

// A rota keeps only people in the project, each once, in turns of 1 to 12
// weeks from a real date, and at most MaxRota people.
func TestRotaPutRefusesPeopleOutsideTheProject(t *testing.T) {
	w := newRotaWorld(t)
	w.role = workAccess.RoleAdmin
	project, ann, ben := uuid.New(), uuid.NewString(), uuid.NewString()
	w.members[ann], w.members[ben] = false, false
	many := make([]string, MaxRota+1)
	for i := range many {
		many[i] = uuid.NewString()
		w.members[many[i]] = false
	}
	for name, bad := range map[string]Rota{
		"an outsider":     {People: []string{ann, uuid.NewString()}, Weeks: 1, Start: "2026-11-02"},
		"not a person":    {People: []string{"ann"}, Weeks: 1, Start: "2026-11-02"},
		"no weeks":        {People: []string{ann}, Weeks: 0, Start: "2026-11-02"},
		"thirteen weeks":  {People: []string{ann}, Weeks: 13, Start: "2026-11-02"},
		"not a date":      {People: []string{ann}, Weeks: 1, Start: "next monday"},
		"too many people": {People: many, Weeks: 1, Start: "2026-11-02"},
	} {
		if _, err := SetRota(context.Background(), member(), w.standing(project), project, bad); !errors.Is(err, ErrBadRota) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(w.kept) != 0 {
		t.Fatalf("a bad rota was kept: %+v", w.kept)
	}
	got, err := SetRota(context.Background(), member(), w.standing(project), project, Rota{People: []string{" " + strings.ToUpper(ann), ben, ann}, Weeks: 2, Start: "2026-11-02"})
	if err != nil || len(w.kept) != 1 || strings.Join(w.kept[0].People, ",") != ann+","+ben {
		t.Fatalf("a rota naming ann twice: %+v, %v, kept %+v", got, err, w.kept)
	}
	if _, err := SetRota(context.Background(), member(), w.standing(project), project, Rota{Weeks: 1, Start: "2026-11-02"}); err != nil {
		t.Errorf("an empty rota (nobody on duty) is a rota too: %v", err)
	}
}

// Work that comes to wait in triage tells the rules engine it arrived, with
// its door and who sent it; work that doesn't wait (triage off, triage full,
// or not kept) tells it nothing.
func TestArrivalFiresTaskArrivedOnlyWhenHeld(t *testing.T) {
	w := newRotaWorld(t)
	ctx, project, task, me := context.Background(), uuid.New(), uuid.New(), uuid.New()
	if held, _ := Hold(ctx, project, task, FromForm, Requester{UserUUID: &me}); held || len(w.arrived) != 0 {
		t.Fatalf("triage off: held %v, told %v", held, w.arrived)
	}
	w.on[project.String()] = true
	if held, err := Hold(ctx, project, task, FromForm, Requester{UserUUID: &me, Name: "Maya"}); !held || err != nil || len(w.arrived) != 1 {
		t.Fatalf("triage on: held %v, %v, told %v", held, err, w.arrived)
	}
	got := w.arrived[0]
	if got["event"] != ArrivedEvent || got["task_id"] != task.String() || got["project_id"] != project.String() ||
		got["source"] != "form" || got["updated_by_uuid"] != me.String() {
		t.Errorf("told %v", got)
	}
	if held, _ := Hold(ctx, project, uuid.New(), FromAPI, Requester{}); !held || len(w.arrived) != 2 || w.arrived[1]["updated_by_uuid"] != nil {
		t.Errorf("someone unknown sent it: told %v", w.arrived)
	}
	w.count = MaxWaiting
	if held, _ := Hold(ctx, project, uuid.New(), FromForm, Requester{}); held || len(w.arrived) != 2 {
		t.Errorf("a full triage told the engine: %v", w.arrived)
	}
	w.count = 0
	put0 := put
	defer func() { put = put0 }()
	put = func(context.Context, []triageModel.Item) error { return errors.New("down") }
	if held, _ := Hold(ctx, project, uuid.New(), FromForm, Requester{}); held || len(w.arrived) != 2 {
		t.Errorf("an item that wasn't kept told the engine: %v", w.arrived)
	}
}

// The engine's on-duty read is the rota alone: one read, no permission (the
// engine checks the rule's owner), nobody when the project keeps none.
func TestOnDutyIsReadOnceForTheEngine(t *testing.T) {
	w := newRotaWorld(t)
	project, ann := uuid.New(), uuid.NewString()
	if who, err := OnDuty(context.Background(), project.String()); who != "" || err != nil || w.reads != 1 {
		t.Fatalf("no rota: %q, %v, %d reads", who, err, w.reads)
	}
	w.rotas[project.String()] = Rota{People: []string{ann}, Weeks: 1, Start: "2026-01-05"}
	if who, err := OnDuty(context.Background(), project.String()); who != ann || err != nil || w.reads != 2 {
		t.Fatalf("a rota: %q, %v, %d reads", who, err, w.reads)
	}
}

// The rota says when the turn next changes (turns change at 00:00 UTC), so
// the panel can show it in the reader's own time: the start for a rota not
// begun, the next whole turn after now, nothing when no one would change.
func TestTheRotaSaysWhenTheTurnChangesNext(t *testing.T) {
	ann, ben := uuid.NewString(), uuid.NewString()
	utc := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	weekly := Rota{People: []string{ann, ben}, Weeks: 1, Start: "2026-11-02"}
	fortnightly := Rota{People: []string{ann, ben}, Weeks: 2, Start: "2026-11-02"}
	for _, c := range []struct {
		name string
		rota Rota
		now  time.Time
		who  string
		next string
	}{
		{"before it starts: the start", weekly, utc("2026-11-01T23:59:59Z"), "", "2026-11-02T00:00:00Z"},
		{"the first turn's first second", weekly, utc("2026-11-02T00:00:00Z"), ann, "2026-11-09T00:00:00Z"},
		{"the first turn's last second", weekly, utc("2026-11-08T23:59:59Z"), ann, "2026-11-09T00:00:00Z"},
		{"on the boundary: the turn after", weekly, utc("2026-11-09T00:00:00Z"), ben, "2026-11-16T00:00:00Z"},
		{"two weeks a turn", fortnightly, utc("2026-11-15T23:00:00Z"), ann, "2026-11-16T00:00:00Z"},
		{"another zone: 05:30 IST on the 9th is the UTC midnight", weekly, time.Date(2026, 11, 9, 5, 29, 0, 0, time.FixedZone("IST", 19800)), ann, "2026-11-09T00:00:00Z"},
		{"one person: no change", Rota{People: []string{ann}, Weeks: 1, Start: "2026-11-02"}, utc("2026-11-20T00:00:00Z"), ann, ""},
		{"one person, not begun: the start", Rota{People: []string{ann}, Weeks: 1, Start: "2026-11-02"}, utc("2026-11-01T00:00:00Z"), "", "2026-11-02T00:00:00Z"},
		{"no one on it: no change", Rota{Weeks: 1, Start: "2026-11-02"}, utc("2026-11-20T00:00:00Z"), "", ""},
	} {
		who, next := onDuty(c.rota, c.now)
		got := ""
		if !next.IsZero() {
			got = next.Format(time.RFC3339)
		}
		if who != c.who || got != c.next {
			t.Errorf("%s: %q until %q, want %q until %q", c.name, who, got, c.who, c.next)
		}
	}
	w := newRotaWorld(t)
	project := uuid.New()
	w.rotas[project.String()] = Rota{People: []string{ann, ben}, Weeks: 1, Start: "2026-10-12"} // the world's clock is 23 Oct
	got, err := ReadRota(context.Background(), w.standing(project), project)
	if err != nil || got.OnDuty != ben || got.NextChange != "2026-10-26T00:00:00Z" {
		t.Fatalf("the view: %+v, %v", got, err)
	}
}

// GET and PUT /project/{p}/triage/rota read the project and the caller's
// standing once a request: the route's check (projectaccess.RequireStanding,
// one project read and one standing read) hands the standing down, and the
// rota's own calls read neither again. can_manage rides the view.
func TestRotaGetAndPutReadTheProjectAndStandingOnce(t *testing.T) {
	w := newRotaWorld(t)
	project, ann := uuid.New(), uuid.NewString()
	w.members[ann] = false
	projects, standings := 0, 0
	project0, standing0 := projectFor, standingIn
	projectFor = func(ctx context.Context, id string, who *userModels.UserInfo) (*dgraphStruct.DgraphProject, error) {
		projects++
		return project0(ctx, id, who)
	}
	standingIn = func(ctx context.Context, who *userModels.UserInfo, p *dgraphStruct.DgraphProject) (workAccess.Standing, error) {
		standings++
		return standing0(ctx, who, p)
	}
	// One request as the route serves it: its one check, then the rota.
	request := func(ctx context.Context, need workAccess.Need, serve func(workAccess.Standing) error) {
		t.Helper()
		projects, standings = 0, 0
		p, err := projectFor(ctx, project.String(), member())
		if err != nil {
			t.Fatal(err)
		}
		s, err := standingIn(ctx, member(), p)
		if err != nil || s.Check(need) != nil {
			t.Fatalf("the route's check: %+v, %v", s, err)
		}
		s.ProjectUUID = project.String()
		if err := serve(s); err != nil {
			t.Fatal(err)
		}
		if projects != 1 || standings != 1 {
			t.Errorf("one request read the project %d times and the standing %d times, want 1 and 1", projects, standings)
		}
	}
	w.role = workAccess.RoleAdmin
	rota := Rota{People: []string{ann}, Weeks: 1, Start: "2026-10-19"}
	request(context.Background(), workAccess.NeedManage, func(s workAccess.Standing) error {
		_, err := SetRota(context.Background(), member(), s, project, rota)
		return err
	})
	request(context.Background(), workAccess.NeedView, func(s workAccess.Standing) error {
		got, err := ReadRota(context.Background(), s, project)
		if err == nil && (!got.CanManage || got.OnDuty != ann) {
			t.Errorf("an admin's read: %+v", got)
		}
		return err
	})
	token := helpers.WithActsThroughToken(context.Background())
	request(token, workAccess.NeedView, func(s workAccess.Standing) error {
		got, err := ReadRota(token, s, project)
		if err == nil && got.CanManage {
			t.Error("an admin's token may manage the rota")
		}
		return err
	})
	w.role = workAccess.RoleViewer
	request(context.Background(), workAccess.NeedView, func(s workAccess.Standing) error {
		got, err := ReadRota(context.Background(), s, project)
		if err == nil && got.CanManage {
			t.Error("a viewer may manage the rota")
		}
		return err
	})
	// A standing checked for another project admits nothing here.
	other := workAccess.Standing{ProjectUUID: uuid.NewString(), Role: workAccess.RoleAdmin}
	if _, err := ReadRota(context.Background(), other, project); !errors.Is(err, taskBusiness.ErrTaskNotVisible) {
		t.Errorf("another project's standing read the rota: %v", err)
	}
	if _, err := SetRota(context.Background(), member(), other, project, rota); !errors.Is(err, taskBusiness.ErrTaskNotVisible) {
		t.Errorf("another project's standing set the rota: %v", err)
	}
}
