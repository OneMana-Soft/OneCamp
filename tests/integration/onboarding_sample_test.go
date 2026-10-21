//go:build integration
// +build integration

package integration_test

// The sample project of a new workspace (design-15, basecamp-44), against
// real Postgres, Dgraph and Redis: made once from the built-in client project
// template into a team made for it, it never answers "start your first
// project", and removing it archives the project and the team it made, and
// nothing else.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheSampleProject -v

import (
	"testing"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	_ "github.com/akashc777/OneCamp/business/Marketplace" // the doc templates the brief is made from
	onboardingBusiness "github.com/akashc777/OneCamp/business/Onboarding"
)

func TestTheSampleProjectIsMadeOnceNeverCountsAndGoesInOneStep(t *testing.T) {
	lr := setupLandReady(t)
	ctx := lr.ctx
	t.Cleanup(func() { _, _ = lr.env.PG.Exec(`DELETE FROM system_configs WHERE key = 'onboarding_sample'`) })

	projectStep := func() bool {
		t.Helper()
		for _, s := range onboardingBusiness.Status(ctx, lr.owner).Steps {
			if s.ID == "project" {
				return s.Done
			}
		}
		t.Fatal("no project step")
		return false
	}

	sample, err := onboardingBusiness.MakeSample(ctx, lr.owner)
	if err != nil || sample.ProjectUUID == "" || !sample.MadeTeam {
		t.Fatalf("made %+v, %v", sample, err)
	}
	var tasks int
	if err := lr.env.PG.QueryRow(`SELECT count(*) FROM tasks WHERE project_id = $1 AND deleted_at IS NULL`, sample.ProjectUUID).Scan(&tasks); err != nil || tasks < 5 {
		t.Fatalf("the sample has %d tasks (%v); the template has more", tasks, err)
	}
	// Its brief: the Project brief template, in the project's own space.
	if sample.DocUUID == "" {
		t.Fatal("the sample has no brief")
	}
	brief, err := docBusiness.GetDocByDocUUID(ctx, sample.DocUUID, lr.owner.UserDgraphInfo.Uid)
	if err != nil || brief == nil || brief.Title != onboardingBusiness.SampleDocTitle {
		t.Fatalf("the brief: %+v, %v", brief, err)
	}
	if brief.DeletedAt != nil && brief.DeletedAt.Year() > 1970 {
		t.Fatal("the brief was made in the trash")
	}
	again, _ := onboardingBusiness.MakeSample(ctx, lr.owner)
	if again.ProjectUUID != sample.ProjectUUID {
		t.Fatal("a second press made a second sample")
	}
	if projectStep() {
		t.Fatal("the sample answered 'start your first project'")
	}

	if err := onboardingBusiness.RemoveSample(ctx, lr.owner); err != nil {
		t.Fatal(err)
	}
	var projectGone, teamGone bool
	if err := lr.env.PG.QueryRow(`SELECT deleted_at IS NOT NULL FROM projects WHERE id = $1`, sample.ProjectUUID).Scan(&projectGone); err != nil || !projectGone {
		t.Fatalf("the sample project wasn't archived (%v)", err)
	}
	if err := lr.env.PG.QueryRow(`SELECT deleted_at IS NOT NULL FROM teams WHERE id = $1`, sample.TeamUUID).Scan(&teamGone); err != nil || !teamGone {
		t.Fatalf("the team the sample made wasn't archived (%v)", err)
	}
	if onboardingBusiness.CurrentSample() != nil {
		t.Fatal("a removed sample is still reported")
	}
	if gone, err := docBusiness.GetDocByDocUUID(ctx, sample.DocUUID, lr.owner.UserDgraphInfo.Uid); err == nil && gone != nil && (gone.DeletedAt == nil || gone.DeletedAt.Year() < 1970) {
		t.Fatal("the sample's brief outlived the sample")
	}
}
