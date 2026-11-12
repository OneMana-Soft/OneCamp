package business

// Releases from Git (P10-1b job 2). A code host's integration (business/GitHub
// today, GitLab next) calls these, as its bot, for the projects whose link to
// the repository syncs it: a tag makes or ships each project's release with
// the tasks its commits name (ShipTag); a build or a deploy reports a stage
// on the releases of its commit or tag (ReportStage).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	releaseModel "github.com/akashc777/OneCamp/models/postgres/Release"
	"github.com/google/uuid"
)

// Seams for tests: the reads and writes a code host's report makes, and the
// transaction a tag's ship and its tasks share.
var (
	shipping  = releaseModel.Shipping
	shipRow   = releaseModel.ShipTag
	keepStage = releaseModel.KeepStage
	inTx      = pgexec.InTx
)

// tagName is the release a tag names: its words on one line, in a name's
// bounds. Pure.
func tagName(tag string) string {
	return helpers.TruncateRunes(strings.Join(strings.Fields(tag), " "), nameMost)
}

// TasksSince is a code host's read of the tasks its repository's commits
// since the tag since name, by project.
type TasksSince func(ctx context.Context, since string) (map[uuid.UUID][]uuid.UUID, error)

// errMoved is a previous tag shipped in a project between the read of a
// tag's tasks and its ship: they are read again.
var errMoved = errors.New("release: the repository shipped another tag in the project meanwhile")

// ShipTag makes or ships, in each of projects, the release for repo's tag
// at commit sha, by the code host's bot (see releaseModel.ShipTag), with the
// tasks named reads in its commits since that project's previous tag (none
// for its first): one read for the projects that share a previous tag. A
// tag that can't name a release ships nothing, nor does a project with a
// live release of its name and another tag (logged). A failed read or write
// is returned, for the delivery to come again: shipTask writes a project's
// release and its tasks together or not at all, so a project shipped before
// the failure isn't read or put in again (Again).
func ShipTag(ctx context.Context, projects []uuid.UUID, repo, tag, sha string, named TasksSince, by uuid.UUID) error {
	t := releaseModel.Tag{Repo: strings.ToLower(repo), Name: tag, Release: tagName(tag), SHA: sha}
	if t.Release == "" || len(tag) > 250 || len(sha) > 64 || len(t.Repo) > 200 {
		helpers.MessageLogs.InfoLog.Printf("release: the tag %q of %s can't name a release", helpers.TruncateRunes(tag, 60), repo)
		return nil
	}
	read := map[string]map[uuid.UUID][]uuid.UUID{} // by previous tag
	for _, project := range projects {
		err := errMoved
		for try := 0; try < 3 && errors.Is(err, errMoved); try++ {
			err = shipTask(ctx, project, t, named, read, by)
		}
		if errors.Is(err, releaseModel.ErrTaken) {
			helpers.MessageLogs.InfoLog.Printf("release: tag %s of %s ships no release in project %s: %s", tag, repo, project, taken(t.Release))
			continue
		}
		if err != nil {
			return fmt.Errorf("couldn't ship tag %s of %s in project %s: %w", tag, repo, project, err)
		}
	}
	return nil
}

// shipTask ships t in project with its tasks: every read first (whether it
// shipped already, its previous tag, named's read of the tasks since, kept
// in read), then the ship and the put-in in one transaction, so a tag that
// shipped (Again) took its tasks then, and a task a person took out since
// stays out. errMoved: the previous tag changed before the ship, which
// wrote nothing. A release that can't take the tasks (full) ships without
// them.
func shipTask(ctx context.Context, project uuid.UUID, t releaseModel.Tag, named TasksSince, read map[string]map[uuid.UUID][]uuid.UUID, by uuid.UUID) error {
	before, err := shipping(ctx, project, t)
	if err != nil || before.Again {
		return err
	}
	byProject, ok := read[before.Since]
	if !ok && before.Since != "" {
		if byProject, err = named(ctx, before.Since); err != nil {
			return err
		}
		read[before.Since] = byProject
	}
	var refused *ReleaseError
	err = inTx(ctx, func(ctx context.Context) error {
		s, err := shipRow(ctx, project, t, by)
		switch {
		case err != nil || s.Again:
			return err
		case s.Since != before.Since:
			return errMoved
		}
		err = addFromGit(ctx, s.Release.ID, byProject[project], by)
		errors.As(err, &refused)
		return err
	})
	if refused == nil {
		return err
	}
	helpers.MessageLogs.InfoLog.Printf("release: tag %s ships in project %s without its tasks: %s", t.Name, project, refused)
	_, err = shipRow(ctx, project, t, by)
	return err
}

// addFromGit puts the tasks a tag's commits named in a release, by the code
// host's bot: they were read as the bot, so they go in as git, at most
// MostTasks. A release that can't take them (past MostTasks, or no longer
// live) takes none: a ReleaseError, and shipTask ships it without them.
func addFromGit(ctx context.Context, release uuid.UUID, tasks []uuid.UUID, by uuid.UUID) error {
	if len(tasks) > MostTasks {
		tasks = tasks[:MostTasks]
	}
	_, err := addRows(ctx, release, tasks, releaseModel.Git, by, MostTasks)
	switch {
	case errors.Is(err, releaseModel.ErrFull):
		return &ReleaseError{fmt.Sprintf("A release holds at most %d tasks.", MostTasks)}
	case errors.Is(err, helpers.ErrNotFound):
		return &ReleaseError{"The release was deleted before its tasks went in."}
	}
	return err
}

// ReportStage keeps what a build or deploy said (s) on the live releases of
// projects that s.Repo shipped as the tag ref or at commit s.SHA, and tells
// each release it changed (release.stage) as the code host's bot (by).
func ReportStage(ctx context.Context, projects []uuid.UUID, ref string, s releaseModel.Stage, by string) error {
	s.Repo = strings.ToLower(s.Repo)
	s.Name = helpers.TruncateRunes(strings.TrimSpace(s.Name), 200)
	s.Status = helpers.TruncateRunes(strings.ToLower(strings.TrimSpace(s.Status)), 32)
	s.SHA, s.URL = helpers.TruncateRunes(s.SHA, 64), helpers.TruncateRunes(s.URL, 2048)
	if s.Kind != "build" && s.Kind != "deploy" || s.Name == "" || s.Status == "" || s.Provider == "" || s.Repo == "" {
		return fmt.Errorf("release: a stage needs a kind (build, deploy), a name, a status, a provider and a repository: %+v", s)
	}
	if s.ReportedAt.IsZero() {
		s.ReportedAt = time.Now()
	}
	changed, err := keepStage(ctx, projects, helpers.TruncateRunes(ref, 250), s)
	if err != nil {
		return err
	}
	for _, id := range changed {
		resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
			Kind: resourceEventBusiness.ReleaseStage, ResourceKind: "release", ResourceUUID: id.String(),
			Actor: resourceEventBusiness.Actor{UserUUID: by}, At: time.Now(),
			Data: map[string]string{"provider": s.Provider, "repo": s.Repo, "kind": s.Kind, "name": s.Name, "status": s.Status},
		})
	}
	return nil
}
