package business

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	projectLinkBusiness "github.com/akashc777/OneCamp/business/ProjectLink"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// aroundFakes stands in for everything the around asks and makes, and keeps
// what was done, in order.
type aroundFakes struct {
	may                   bool
	teamErr               error
	takenProjects         map[string]bool
	takenChannels         map[string]bool
	failAt                string // "project", "channel", "link", "post"
	failErr               error
	undoFails             string // "project": its archive fails
	did                   []string
	projectReads, chReads int
	// The team each step was asked about: the one checked, the one whose
	// project names were read, and the project's team (its Dgraph team's uuid
	// and the team id it was made with).
	checkedTeam, namesTeam, projectTeam, projectTeamID string
}

func (f *aroundFakes) install(t *testing.T) {
	t.Helper()
	old := []any{mayMakeProjectIn, takenProjectNames, takenChannelNames, makeAroundProject, makeAroundChannel,
		linkProjectTables, postInChannel, archiveAroundProj, archiveAroundChan, dropInstalledTables, appBase}
	t.Cleanup(func() {
		mayMakeProjectIn = old[0].(func(context.Context, string, *userModels.UserInfo) (*dgraphStruct.DgraphTeam, bool, error))
		takenProjectNames = old[1].(func(context.Context, uuid.UUID, []string) (map[string]bool, error))
		takenChannelNames = old[2].(func(context.Context, []string) (map[string]bool, error))
		makeAroundProject = old[3].(func(context.Context, string, *userModels.UserInfo, *dgraphStruct.DgraphTeam, uuid.UUID) (*dgraphStruct.DgraphProject, error))
		makeAroundChannel = old[4].(func(context.Context, string, *userModels.UserInfo) (uuid.UUID, error))
		linkProjectTables = old[5].(func(context.Context, uuid.UUID, uuid.UUID, []projectLinkBusiness.Named) ([]projectLinkBusiness.View, error))
		postInChannel = old[6].(func(context.Context, *userModels.UserInfo, uuid.UUID, string) error)
		archiveAroundProj = old[7].(func(context.Context, uuid.UUID, *dgraphStruct.DgraphProject) error)
		archiveAroundChan = old[8].(func(context.Context, uuid.UUID, string) error)
		dropInstalledTables = old[9].(func(context.Context, []uuid.UUID, dataTableBusiness.Actor) error)
		appBase = old[10].(func() string)
	})
	fail := func(step string) error {
		if f.failAt == step {
			if f.failErr != nil {
				return f.failErr
			}
			return errors.New(step + " broke")
		}
		return nil
	}
	mayMakeProjectIn = func(_ context.Context, team string, _ *userModels.UserInfo) (*dgraphStruct.DgraphTeam, bool, error) {
		f.did = append(f.did, "read team")
		f.checkedTeam = team
		if f.teamErr != nil {
			return nil, false, f.teamErr
		}
		return &dgraphStruct.DgraphTeam{Uid: "0x1", Uuid: team, Name: "Ops"}, f.may, nil
	}
	takenProjectNames = func(_ context.Context, team uuid.UUID, names []string) (map[string]bool, error) {
		f.projectReads++
		f.namesTeam = team.String()
		f.did = append(f.did, "read projects")
		out := map[string]bool{}
		for _, n := range names {
			if f.takenProjects[n] {
				out[n] = true
			}
		}
		return out, nil
	}
	takenChannelNames = func(_ context.Context, names []string) (map[string]bool, error) {
		f.chReads++
		f.did = append(f.did, "read channels")
		out := map[string]bool{}
		for _, n := range names {
			if f.takenChannels[n] {
				out[n] = true
			}
		}
		return out, nil
	}
	makeAroundProject = func(_ context.Context, name string, _ *userModels.UserInfo, team *dgraphStruct.DgraphTeam, teamID uuid.UUID) (*dgraphStruct.DgraphProject, error) {
		f.did = append(f.did, "make project "+name)
		if team != nil {
			f.projectTeam = team.Uuid
		}
		f.projectTeamID = teamID.String()
		if err := fail("project"); err != nil {
			return nil, err
		}
		return &dgraphStruct.DgraphProject{Uuid: uuid.NewString(), Name: name}, nil
	}
	makeAroundChannel = func(_ context.Context, name string, _ *userModels.UserInfo) (uuid.UUID, error) {
		f.did = append(f.did, "make channel "+name)
		return uuid.New(), fail("channel")
	}
	linkProjectTables = func(_ context.Context, _, _ uuid.UUID, links []projectLinkBusiness.Named) ([]projectLinkBusiness.View, error) {
		titles := make([]string, 0, len(links))
		for _, l := range links {
			titles = append(titles, l.Title)
			if !strings.HasPrefix(l.URL, "https://work.example.com/app/tables/") {
				t.Errorf("a table's address: %s", l.URL)
			}
		}
		f.did = append(f.did, "link "+strings.Join(titles, " "))
		return nil, fail("link")
	}
	postInChannel = func(_ context.Context, _ *userModels.UserInfo, _ uuid.UUID, body string) error {
		f.did = append(f.did, "post")
		if !strings.Contains(body, "/app/project/") || !strings.Contains(body, ">Deals</a>") {
			t.Errorf("the first message: %s", body)
		}
		return fail("post")
	}
	archiveAroundProj = func(context.Context, uuid.UUID, *dgraphStruct.DgraphProject) error {
		f.did = append(f.did, "archive project")
		if f.undoFails == "project" {
			return errors.New("the archive broke too")
		}
		return nil
	}
	archiveAroundChan = func(_ context.Context, _ uuid.UUID, name string) error {
		f.did = append(f.did, "archive channel "+name)
		return nil
	}
	dropInstalledTables = func(_ context.Context, ids []uuid.UUID, _ dataTableBusiness.Actor) error {
		short := make([]string, 0, len(ids))
		for _, id := range ids {
			short = append(short, id.String()[:1])
		}
		f.did = append(f.did, "drop tables "+strings.Join(short, " "))
		return nil
	}
	appBase = func() string { return "https://work.example.com/" }
}

var (
	deals    = madeTable{ID: uuid.MustParse("a0000000-0000-4000-8000-000000000001"), Name: "Deals"}
	contacts = madeTable{ID: uuid.MustParse("b0000000-0000-4000-8000-000000000002"), Name: "Contacts"}
	aTeam    = uuid.MustParse("c0000000-0000-4000-8000-000000000003").String()
)

func installsCRM(f *aroundFakes) func() (*InstallResult, []madeTable, error) {
	return func() (*InstallResult, []madeTable, error) {
		f.did = append(f.did, "make tables")
		return &InstallResult{Kind: "table", EntityID: deals.ID.String(), Name: "CRM"}, []madeTable{deals, contacts}, nil
	}
}

func someone() *userModels.UserInfo {
	u := &userModels.UserInfo{}
	u.UserPostgresInfo.Id = uuid.New()
	return u
}

func same(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Errorf("%s:\n got %v\nwant %v", what, got, want)
	}
}

// Everything the around needs is read and checked before anything is made:
// someone who may not make projects in the team, a team that can't be read,
// a team that isn't one, and a name no channel may have make nothing.
func TestTemplateAroundChecksBeforeAnyWrite(t *testing.T) {
	cases := []struct {
		name, team, template string
		f                    aroundFakes
		status               int
		did                  []string
	}{
		{"may not make projects", aTeam, "CRM", aroundFakes{may: false}, http.StatusForbidden, []string{"read team"}},
		{"the team can't be read", aTeam, "CRM", aroundFakes{teamErr: errors.New("graph down")}, 0, []string{"read team"}},
		{"no team", "not-a-team", "CRM", aroundFakes{may: true}, http.StatusBadRequest, nil},
		{"a name no channel may have", aTeam, "Q&A", aroundFakes{may: true}, http.StatusBadRequest, []string{"read team", "read projects", "read channels"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := c.f
			f.install(t)
			res, err := withAround(context.Background(), someone(), InstallOptions{ProjectTeam: c.team}, c.template, installsCRM(&f))
			if err == nil || res != nil {
				t.Fatalf("went on: %+v %v", res, err)
			}
			if r, ok := AsRefusal(err); c.status != 0 && (!ok || r.Status != c.status) {
				t.Errorf("refusal: %v, want %d", err, c.status)
			}
			same(t, "what was done", f.did, c.did...)
		})
	}
}

// A step that fails after others undoes them, last made first (the channel,
// the project, then the tables, all of them in one delete), answers its own
// error, and makes nothing twice. An undo that fails too still answers the
// step's error. The tables are linked on the project in one write.
func TestTemplateAroundUndoesWhatItMadeWhenALaterStepFails(t *testing.T) {
	tables := "drop tables a b"
	cases := []struct {
		failAt, undoFails string
		did               []string
	}{
		{"project", "", []string{"make project CRM", tables}},
		{"channel", "", []string{"make project CRM", "make channel CRM", "archive project", tables}},
		{"link", "", []string{"make project CRM", "make channel CRM", "link Deals Contacts", "archive channel CRM", "archive project", tables}},
		{"post", "", []string{"make project CRM", "make channel CRM", "link Deals Contacts", "post", "archive channel CRM", "archive project", tables}},
		{"post", "project", []string{"make project CRM", "make channel CRM", "link Deals Contacts", "post", "archive channel CRM", "archive project", tables}},
	}
	for _, c := range cases {
		t.Run(c.failAt+" "+c.undoFails, func(t *testing.T) {
			broke := errors.New(c.failAt + " broke")
			f := aroundFakes{may: true, failAt: c.failAt, failErr: broke, undoFails: c.undoFails}
			f.install(t)
			res, err := withAround(context.Background(), someone(), InstallOptions{ProjectTeam: aTeam}, "CRM", installsCRM(&f))
			if res != nil || !errors.Is(err, broke) {
				t.Errorf("answered %+v, %v; want the %s's own error", res, err, c.failAt)
			}
			want := append([]string{"read team", "read projects", "read channels", "make tables"}, c.did...)
			same(t, "what was done and undone", f.did, want...)
		})
	}
}

// A project or channel named just now, between the read and the make, is a
// refusal the installer can act on, and what was made is undone.
func TestTemplateAroundNameTakenJustNowIsAConflict(t *testing.T) {
	f := aroundFakes{may: true, failAt: "channel", failErr: &pgconn.PgError{Code: "23505"}}
	f.install(t)
	_, err := withAround(context.Background(), someone(), InstallOptions{ProjectTeam: aTeam}, "CRM", installsCRM(&f))
	if r, ok := AsRefusal(err); !ok || r.Status != http.StatusConflict || !strings.Contains(r.Msg, "“CRM”") {
		t.Errorf("answered %v", err)
	}
	if f.did[len(f.did)-1] != "drop tables a b" {
		t.Errorf("not undone: %v", f.did)
	}
}

// The project and the channel are named by the template, numbered past the
// names taken (each in one read), and the install answers both.
func TestTemplateAroundNamesPastTakenOnes(t *testing.T) {
	f := aroundFakes{may: true, takenProjects: map[string]bool{"CRM": true, "CRM 2": true}, takenChannels: map[string]bool{"CRM": true}}
	f.install(t)
	res, err := withAround(context.Background(), someone(), InstallOptions{ProjectTeam: aTeam}, "CRM", installsCRM(&f))
	if err != nil {
		t.Fatal(err)
	}
	if res.ProjectName != "CRM 3" || res.ChannelName != "CRM 2" || res.ProjectID == "" || res.ChannelID == "" || res.EntityID != deals.ID.String() {
		t.Errorf("answered %+v", res)
	}
	if f.projectReads != 1 || f.chReads != 1 {
		t.Errorf("names read %d and %d times, want once each", f.projectReads, f.chReads)
	}
	same(t, "what was done", f.did, "read team", "read projects", "read channels", "make tables",
		"make project CRM 3", "make channel CRM 2", "link Deals Contacts", "post")
	// Every step is about the team asked for: the one checked, the one whose
	// names are read, and the one the project goes in.
	same(t, "the teams", []string{f.checkedTeam, f.namesTeam, f.projectTeam, f.projectTeamID}, aTeam, aTeam, aTeam, aTeam)
}

// Without the option the install is the one it always was: nothing more is
// read or made.
func TestTemplateInstallWithoutAroundIsUnchanged(t *testing.T) {
	f := aroundFakes{may: true}
	f.install(t)
	res, err := withAround(context.Background(), someone(), InstallOptions{ProjectTeam: "  "}, "CRM", installsCRM(&f))
	if err != nil || res.ProjectID != "" || res.ChannelID != "" {
		t.Errorf("answered %+v, %v", res, err)
	}
	same(t, "what was done", f.did, "make tables")
}

// Only a table template makes a project and a channel: a doc template asked
// for them is refused before anything is read or made.
func TestTemplateAroundOnlyForTableTemplates(t *testing.T) {
	f := aroundFakes{may: true}
	f.install(t)
	_, err := Install(context.Background(), BuiltinDocTemplates[0].ID, someone(), InstallOptions{ProjectTeam: aTeam})
	if r, ok := AsRefusal(err); !ok || r.Status != http.StatusBadRequest {
		t.Errorf("a doc template with a project: %v", err)
	}
	same(t, "what was done", f.did)
}
