package business

// The sample project: a worked project a new owner can open, then remove in
// one step (design-15, basecamp-44; design/platform/ONBOARDING_ANALYSIS.md).
//
// WHY. A new workspace's first project is an empty list, and an empty list
// shows nothing of what a project does. Basecamp opens with a worked sample.
// This is the same thing, made the way any project is made: the built-in
// "Client project" template (business/ProjectTemplate) applied to an
// ordinary project, so it behaves exactly as real work does.
//
// BY ITS ORIGIN. What the sample made is written down when it is made: the
// project, its brief, and the team when the workspace had none to put it in.
// Removing the sample takes exactly those away (the brief to the trash, the
// project and team archived, the way a person does), so it can never touch
// real work; and the sample never counts as "your first project" in the
// checklist, which stays derived from the workspace.
//
// THE BRIEF. A doc the project starts from, "Start here: the brief", in the
// project's own space: OneCamp's own "Project brief" doc template, so the run
// shows a doc beside the tasks (DESIGN-BUILD-3, design-15's way to WIN).

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	templateBusiness "github.com/akashc777/OneCamp/business/ProjectTemplate"
	teamBusiness "github.com/akashc777/OneCamp/business/Team"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// sampleKey holds the one sample's record, workspace-wide.
const sampleKey = "onboarding_sample"

// SampleTemplateID is the built-in template the sample is made from.
const SampleTemplateID = "client-project"

// SampleName is the sample project's name, which says what it is.
const SampleName = "Sample: a client project"

// sampleTeamName names the team made for the sample when there was none.
const sampleTeamName = "Our team"

// SampleDocTitle is the sample's brief's title.
const SampleDocTitle = "Start here: the brief"

// StartHereTemplateID is OneCamp's own "Project brief" doc template
// (business/Marketplace BuiltinDocTemplates; its id is fixed). Asked for by id:
// this package can't import the marketplace, which imports the agents, which
// import this package. sampleBrief_test.go holds it to the built-in list.
const StartHereTemplateID = "b5d0c000-0000-4000-8000-000000000002"

// Sample is what the sample made, and so what removing it archives.
type Sample struct {
	ProjectUUID string `json:"project_uuid"`
	TeamUUID    string `json:"team_uuid"`
	MadeTeam    bool   `json:"made_team,omitempty"`
	// DocUUID is the brief, when it could be made.
	DocUUID   string    `json:"doc_uuid,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Removed   bool      `json:"removed,omitempty"`
}

// ErrNoSample is a removal with no sample to remove.
var ErrNoSample = errors.New("onboarding: there is no sample project")

// Seams for the tests: the parts that need Postgres and Dgraph.
var (
	loadSample = func() (*Sample, error) {
		cfg, err := configModels.GetConfigByKey(sampleKey)
		if err != nil || cfg == nil || cfg.Value == "" {
			return nil, err
		}
		var s Sample
		if json.Unmarshal([]byte(cfg.Value), &s) != nil {
			return nil, nil
		}
		return &s, nil
	}
	saveSample = func(s Sample) error {
		b, err := json.Marshal(s)
		if err != nil {
			return err
		}
		return configModels.UpsertConfig(sampleKey, string(b))
	}
	// makeSample makes the team (when needed) and the project, and applies the
	// template, as the admin.
	makeSample = func(ctx context.Context, admin userModels.UserInfo) (Sample, error) {
		s := Sample{CreatedAt: time.Now()}
		teams, err := teamBusiness.GetDgraphTeamListByAdminDgraphUID(ctx, admin.UserDgraphInfo.Uid)
		if err != nil {
			return s, err
		}
		var team = firstLiveTeam(teams)
		if team == nil {
			if team, err = teamBusiness.CreateTeam(ctx, sampleTeamName, &admin); err != nil {
				return s, err
			}
			s.MadeTeam = true
		}
		teamUUID, err := uuid.Parse(team.Uuid)
		if err != nil {
			return s, err
		}
		s.TeamUUID = team.Uuid
		// Read back as the create-project route reads it: what CreateTeam and the
		// team list hand back aren't the node a project is made under.
		if team, err = teamBusiness.GetBasicDgraphTeamInfoByUUID(ctx, team.Uuid, admin.UserDgraphInfo.Uid); err != nil || team == nil || team.Uid == "" {
			if err == nil {
				err = errors.New("onboarding: the sample's team can't be read")
			}
			return s, err
		}
		project, err := projectBusiness.CreateProject(ctx, SampleName, admin.UserDgraphInfo.Uid, admin.UserPostgresInfo.Id, team, teamUUID, &admin.UserDgraphInfo)
		if err != nil {
			return s, err
		}
		s.ProjectUUID = project.Uuid
		t, err := templateBusiness.Get(ctx, SampleTemplateID)
		if err != nil {
			return s, err
		}
		applied := templateBusiness.ApplyTo(ctx, t, project.Uuid, &admin, templateBusiness.StartOn("", "", true))
		if applied.Failed > 0 {
			helpers.LogErrorWithContext(ctx, "business/Onboarding/makeSample: %d of the template's items weren't made", applied.Failed)
		}
		// The brief, in the project's space, open to whoever the project is.
		// A sample without one is still a sample: said, not failed.
		open := false
		doc, err := docBusiness.CreateDocInTree(ctx, &admin.UserDgraphInfo, docBusiness.NewDoc{
			Title: SampleDocTitle, Private: &open, Space: "project:" + project.Uuid, TemplateID: StartHereTemplateID,
		}, nil)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Onboarding/makeSample: the sample's brief wasn't made: %v", err)
		} else if doc != nil {
			s.DocUUID = doc.Uuid
		}
		return s, nil
	}
	// trashDoc puts the sample's brief in the trash, as the admin, as a person
	// deletes a doc.
	trashDoc = func(ctx context.Context, docUUID string, admin userModels.UserInfo) error {
		_, err := docBusiness.TrashSubtree(ctx, docUUID, admin.UserDgraphInfo.Uid, admin.UserPostgresInfo.Id.String(), admin.UserPostgresInfo.EmailID)
		return err
	}
	// archiveSample archives what the sample made, as an admin archives it.
	archiveSample = func(ctx context.Context, admin userModels.UserInfo, s Sample) error {
		projectUUID, err := uuid.Parse(s.ProjectUUID)
		if err != nil {
			return err
		}
		info, err := projectBusiness.GetDgraphProjectInfoAndTeamAdminFlag(ctx, s.ProjectUUID, admin.UserDgraphInfo.Uid)
		if err != nil {
			return err
		}
		if err := projectBusiness.ArchiveProjectByProjectUUID(ctx, projectUUID, info); err != nil {
			return err
		}
		if !s.MadeTeam {
			return nil
		}
		// The team the sample made goes too, unless someone put real work in it.
		teamUUID, err := uuid.Parse(s.TeamUUID)
		if err != nil {
			return err
		}
		team, err := teamBusiness.GetDgraphTeamInfoByUUIDForArchivingAndUnarchivingTeam(ctx, s.TeamUUID)
		if err != nil || team == nil || hasLiveProjectBesides(team.Projects, s.ProjectUUID) {
			return err
		}
		return teamBusiness.ArchiveTeamByTeamUUID(ctx, teamUUID, team)
	}
)

// MakeSample makes the sample project, once: a workspace that has one (live)
// is answered with it.
func MakeSample(ctx context.Context, admin userModels.UserInfo) (Sample, error) {
	if s, err := loadSample(); err == nil && s != nil && !s.Removed {
		return *s, nil
	}
	s, err := makeSample(ctx, admin)
	if s.ProjectUUID == "" {
		return s, err
	}
	// Recorded even when a later part failed: what was made can be removed.
	if serr := saveSample(s); serr != nil {
		return s, serr
	}
	return s, err
}

// RemoveSample archives what the sample made, and nothing else.
func RemoveSample(ctx context.Context, admin userModels.UserInfo) error {
	s, err := loadSample()
	if err != nil {
		return err
	}
	if s == nil || s.Removed || s.ProjectUUID == "" {
		return ErrNoSample
	}
	// The brief first: one already in the trash, or gone, is no reason to keep the rest.
	if s.DocUUID != "" {
		if err := trashDoc(ctx, s.DocUUID, admin); err != nil && !errors.Is(err, docBusiness.ErrDocDeleted) && !errors.Is(err, docBusiness.ErrNotFound) {
			return err
		}
	}
	if err := archiveSample(ctx, admin, *s); err != nil {
		return err
	}
	s.Removed = true
	return saveSample(*s)
}

// CurrentSample is the live sample, or nil.
func CurrentSample() *Sample {
	s, err := loadSample()
	if err != nil || s == nil || s.Removed {
		return nil
	}
	return s
}

// sampleIs says whether a project is the live sample, so a checklist step
// asks about real work only.
func sampleIs(s *Sample, projectUUID string) bool {
	return s != nil && s.ProjectUUID == projectUUID
}

// live says a node isn't archived: a live one's deletion time is unset or the
// zero time (0001-01-01), never a date after 1970.
func live(deletedAt *time.Time) bool {
	return deletedAt == nil || deletedAt.Year() < 1970
}

// firstLiveTeam is the first team that isn't archived, or nil.
func firstLiveTeam(teams []*dgraphStruct.DgraphTeam) *dgraphStruct.DgraphTeam {
	for _, t := range teams {
		if t != nil && t.Uuid != "" && live(t.DeletedAt) {
			return t
		}
	}
	return nil
}

// hasLiveProjectBesides says whether a team holds live work other than the
// sample, which would keep the team.
func hasLiveProjectBesides(projects []*dgraphStruct.DgraphProject, sampleUUID string) bool {
	for _, p := range projects {
		if p != nil && p.Uuid != sampleUUID && live(p.DeletedAt) {
			return true
		}
	}
	return false
}
