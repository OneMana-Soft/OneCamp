package business

// A table template that also makes the project and the channel around its
// tables (gsheets-structure-05, item 2). Asked for on the install
// (InstallOptions.ProjectTeam), it makes, beside the tables:
//   - a project in that team, named by the template, the installer its admin,
//     whose home links each of the template's tables (business/ProjectLink);
//   - a channel named by the template, the installer in it, whose first
//     message links the tables and the project.
//
// Who may: the tables as before (any member); the project by the workspace's
// "Who can make projects" for the team (projectBusiness.MayMakeProjectIn, as
// the project's own create asks); the channel, any member, under a valid
// name (helpers.IsValidName, as the channel's own create asks). Every check
// and both names are read before the first write. A step that fails after
// others undoes them, last made first, through their soft deletes (the
// channel archived, the project archived, each table deleted to the trash),
// and the step's own error is answered; an undo that fails is logged as the
// orphan it leaves. Nothing is sent again by itself.

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	projectLinkBusiness "github.com/akashc777/OneCamp/business/ProjectLink"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/google/uuid"
)

// InstallOptions is what an install is asked beyond its template.
type InstallOptions struct {
	// ProjectTeam, when set, asks a table template for a project in this team
	// and a channel around its tables. Empty is the install as it always was.
	ProjectTeam string `json:"project_team_uuid,omitempty"`
}

// Refusal is an install refused in words, with its HTTP status.
type Refusal struct {
	Status int
	Msg    string
}

func (r *Refusal) Error() string { return r.Msg }

// AsRefusal is err as a Refusal, when it is one: the install's own, or what
// the kind's create refused in the person's words (a helpers.Refusal: an
// agent's model or remote, a workflow's scope, a table's name), with its
// status (403 when it names none) and only its words (CR-444 D1).
func AsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	if errors.As(err, &r) {
		return r, true
	}
	var kind *helpers.Refusal
	if !errors.As(err, &kind) {
		return nil, false
	}
	status := kind.Status
	if status == 0 {
		status = http.StatusForbidden
	}
	return &Refusal{status, kind.Msg}, true
}

// numberedNamesTried is how many numbered names ("CRM 2" ...) one read asks
// after; past them a name taken just now is a refusal, not a guess.
const numberedNamesTried = 50

// madeTable is a table an install made: its id and its name.
type madeTable struct {
	ID   uuid.UUID
	Name string
}

// Seams for tests: what the around asks and makes, each the app's own.
var (
	mayMakeProjectIn    = projectBusiness.MayMakeProjectIn
	takenProjectNames   = projectBusiness.TakenProjectNames
	takenChannelNames   = channelBusiness.TakenChannelNames
	makeAroundProject   = makeProjectAs
	makeAroundChannel   = makeChannelAs
	linkProjectTables   = projectLinkBusiness.AddNamed
	postInChannel       = postAs
	archiveAroundProj   = projectBusiness.ArchiveProjectByProjectUUID
	archiveAroundChan   = archiveChannel
	dropInstalledTables = dataTableBusiness.DeleteTables
	appBase             = authService.FrontendBaseURL
)

func makeProjectAs(ctx context.Context, name string, who *userModels.UserInfo, team *dgraphStruct.DgraphTeam, teamID uuid.UUID) (*dgraphStruct.DgraphProject, error) {
	return projectBusiness.CreateProject(ctx, name, who.UserDgraphInfo.Uid, who.UserPostgresInfo.Id, team, teamID, &who.UserDgraphInfo)
}

func makeChannelAs(ctx context.Context, name string, who *userModels.UserInfo) (uuid.UUID, error) {
	err, id := channelBusiness.CreateChannel(ctx, &channelAdapter.InputCreateChannel{ChannelName: name}, who)
	return id, err
}

func postAs(ctx context.Context, who *userModels.UserInfo, channel uuid.UUID, body string) error {
	post, err := sendBusiness.PrepareChannelPost(ctx, who, &postAdapter.InputCreateOrUpdatePostInfo{ChannelUuid: channel.String(), HTMLText: body})
	if err != nil {
		return err
	}
	_, err = post.Commit(ctx)
	return err
}

// archiveChannel archives a channel made by an install that couldn't finish:
// the channel's own archive, its soft delete.
func archiveChannel(ctx context.Context, id uuid.UUID, name string) error {
	return channelBusiness.UpdateChannelInfo(ctx, &channelAdapter.UpdateChannelInfo{ChannelUuid: id.String(), ChannelName: name, ChannelArchived: true}, id)
}

// around is what an install will make around its tables, read and checked
// before anything is made.
type around struct {
	who     *userModels.UserInfo
	team    *dgraphStruct.DgraphTeam
	teamID  uuid.UUID
	project string // its name
	channel string // its name
}

// planAround reads and checks everything the around needs, writing nothing:
// the team, whether who may make projects in it, and the project's and the
// channel's names, the template's, numbered past taken ones (one read each).
func planAround(ctx context.Context, who *userModels.UserInfo, teamUUID, name string) (*around, error) {
	teamID, err := uuid.Parse(strings.TrimSpace(teamUUID))
	if err != nil {
		return nil, &Refusal{http.StatusBadRequest, "Choose a team for the project."}
	}
	team, may, err := mayMakeProjectIn(ctx, teamID.String(), who)
	if err != nil {
		return nil, fmt.Errorf("couldn't read the team: %w", err)
	}
	if !may || team == nil {
		return nil, &Refusal{http.StatusForbidden, "Only a team's admins can make projects in it. Use the template without a project, or ask one of them."}
	}
	name = strings.TrimSpace(name)
	names := make([]string, 0, numberedNamesTried+1)
	names = append(names, name)
	for n := 2; n <= numberedNamesTried+1; n++ {
		names = append(names, fmt.Sprintf("%s %d", name, n))
	}
	projects, err := takenProjectNames(ctx, teamID, names)
	if err != nil {
		return nil, fmt.Errorf("couldn't read the team's projects: %w", err)
	}
	channels, err := takenChannelNames(ctx, names)
	if err != nil {
		return nil, fmt.Errorf("couldn't read the channels: %w", err)
	}
	a := &around{who: who, team: team, teamID: teamID,
		project: helpers.UniqueName(name, lower(projects)), channel: helpers.UniqueName(name, lower(channels))}
	if !helpers.IsValidName(a.channel) {
		return nil, &Refusal{http.StatusBadRequest, "A channel can't be named “" + a.channel + "”. Use the template without a project and a channel."}
	}
	return a, nil
}

func lower(taken map[string]bool) map[string]bool {
	out := make(map[string]bool, len(taken))
	for n := range taken {
		out[strings.ToLower(n)] = true
	}
	return out
}

// make makes the project and the channel around tables (the first is the one
// the install opens), and their links, into res. A step that fails undoes
// what was made before it, the tables too, and its error is answered.
func (a *around) make(ctx context.Context, tables []madeTable, res *InstallResult) (err error) {
	var project *dgraphStruct.DgraphProject
	var projectID, channelID uuid.UUID
	defer func() {
		if err == nil {
			return
		}
		if channelID != uuid.Nil {
			_ = helpers.CompensateOnFailure(ctx, "channel "+a.channel+" of a table template", func(c context.Context) error {
				return archiveAroundChan(c, channelID, a.channel)
			})
		}
		if projectID != uuid.Nil {
			_ = helpers.CompensateOnFailure(ctx, "project "+a.project+" of a table template", func(c context.Context) error {
				return archiveAroundProj(c, projectID, project)
			})
		}
		undoTables(ctx, a.who, tables)
	}()

	project, err = makeAroundProject(ctx, a.project, a.who, a.team, a.teamID)
	if err == nil && (project == nil || project.Uuid == "") {
		err = errors.New("no project was made")
	}
	if err != nil {
		return named(err, "project", a.project)
	}
	if projectID, err = uuid.Parse(project.Uuid); err != nil {
		return err
	}
	if channelID, err = makeAroundChannel(ctx, a.channel, a.who); err != nil {
		channelID = uuid.Nil
		return named(err, "channel", a.channel)
	}
	base := strings.TrimRight(appBase(), "/")
	named := make([]projectLinkBusiness.Named, 0, len(tables))
	links := make([]string, 0, len(tables))
	for _, t := range tables {
		address := base + "/app/tables/" + t.ID.String()
		named = append(named, projectLinkBusiness.Named{URL: address, Title: t.Name})
		links = append(links, `<a href="`+html.EscapeString(address)+`">`+html.EscapeString(t.Name)+`</a>`)
	}
	// Each table is a link on the project's home, all in one write.
	if _, err = linkProjectTables(ctx, projectID, a.who.UserPostgresInfo.Id, named); err != nil {
		return fmt.Errorf("couldn't link the tables on the project: %w", err)
	}
	projectAddress := base + "/app/project/" + projectID.String()
	body := "<p>This channel is for the " + html.EscapeString(res.Name) + " tables: " + strings.Join(links, ", ") +
		`.</p><p>Their project: <a href="` + html.EscapeString(projectAddress) + `">` + html.EscapeString(a.project) + `</a>.</p>`
	if err = postInChannel(ctx, a.who, channelID, body); err != nil {
		return fmt.Errorf("couldn't post in the channel: %w", err)
	}
	res.ProjectID, res.ProjectName = projectID.String(), a.project
	res.ChannelID, res.ChannelName = channelID.String(), a.channel
	return nil
}

// named is a create's failure in words: a name taken since it was read is
// one the installer can act on.
func named(err error, kind, name string) error {
	if helpers.IsUniqueViolation(err) {
		return &Refusal{http.StatusConflict, "A " + kind + " named “" + name + "” was made just now. Try again."}
	}
	return fmt.Errorf("couldn't make the %s: %w", kind, err)
}

// undoTables deletes, to the trash, the tables an install made and couldn't
// finish around, all in one write.
func undoTables(ctx context.Context, who *userModels.UserInfo, tables []madeTable) {
	if len(tables) == 0 {
		return
	}
	actor := dataTableBusiness.Actor{UserID: who.UserPostgresInfo.Id, IsAdmin: who.UserPostgresInfo.IsAdmin}
	ids := make([]uuid.UUID, 0, len(tables))
	for _, t := range tables {
		ids = append(ids, t.ID)
	}
	_ = helpers.CompensateOnFailure(ctx, "the tables of a table template", func(c context.Context) error {
		return dropInstalledTables(c, ids, actor)
	})
}

// withAround installs a table template, and, asked (opts.ProjectTeam), the
// project and channel around its tables: checked before install runs, made
// after it.
func withAround(ctx context.Context, who *userModels.UserInfo, opts InstallOptions, name string, install func() (*InstallResult, []madeTable, error)) (*InstallResult, error) {
	if strings.TrimSpace(opts.ProjectTeam) == "" {
		res, _, err := install()
		return res, err
	}
	plan, err := planAround(ctx, who, opts.ProjectTeam, name)
	if err != nil {
		return nil, err
	}
	res, tables, err := install()
	if err != nil {
		return nil, err
	}
	if err := plan.make(ctx, tables, res); err != nil {
		return nil, err
	}
	return res, nil
}

// errAroundNotTable is a project and channel asked of a template that isn't
// a table's.
var errAroundNotTable = &Refusal{http.StatusBadRequest, "Only a table template makes a project and a channel."}
