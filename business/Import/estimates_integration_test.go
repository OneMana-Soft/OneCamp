//go:build integration

package business

// linear-05: a project from Linear estimates on its team's scale, and each
// issue's estimate comes over as its task's points.
// Run: go test -tags=integration ./business/Import/ -run TestAProjectFromLinearKeepsItsTeamsScaleAndPoints -v

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	estimateScale "github.com/akashc777/OneCamp/business/EstimateScale"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func TestAProjectFromLinearKeepsItsTeamsScaleAndPoints(t *testing.T) {
	ctx := context.Background()
	env, dg := importEnv(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	by, launch, plain := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email_id, created_at, updated_at) VALUES ($1, 'maya@example.com', NOW(), NOW())`, by)
	exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $3), ($2, 'Plain', $3)`, launch, plain, by)
	live := "0001-01-01T00:00:00Z"
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": by.String(), "user_name": "maya", "user_deleted_at": live},
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": uuid.NewString(), "team_name": "Eng", "team_deleted_at": live},
		{"uid": "_:p", "dgraph.type": "Project", "project_uuid": launch.String(), "project_name": "Launch", "project_deleted_at": live, "project_team": map[string]any{"uid": "_:team"}},
	})
	job := &importModels.Job{Id: uuid.New(), Provider: "linear", SourceWorkspaceName: "w", Source: "api", Status: importModels.StatusRunning}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	keepEstimateScale(ctx, job, &importProvider.SourceProject{SourceID: "launch",
		EstimateScale: &importProvider.SourceEstimateScale{Kind: "tshirt", Extended: true, CountUnestimated: true}}, launch, by)
	got, err := estimateScale.Of(ctx, launch)
	if err != nil || got == nil || got.Kind != "tshirt" || !got.Extended || got.AllowZero || !got.CountUnestimated || got.SetBy == nil || *got.SetBy != by {
		t.Fatalf("Launch estimates on its team's T-shirt sizes: %+v %v", got, err)
	}

	maya := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: by}, UserDgraphInfo: dgraphStruct.DgraphUser{Uid: uids["maya"], Uuid: by.String()}}
	write := func(name string, points *float64) uuid.UUID {
		t.Helper()
		id, _, err := writeTask(ctx, job, importTaskInput{
			ProjectUUID: launch, Project: &dgraphStruct.DgraphProject{Uid: uids["p"], Uuid: launch.String(), Team: &dgraphStruct.DgraphTeam{Uid: uids["team"]}},
			Creator: maya, Importer: maya, Name: name, Status: "todo", Priority: "medium", CreatedAt: time.Now().UTC(), EstimatePoints: points,
		})
		if err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return id
	}
	pointsOf := func(task uuid.UUID) *float64 {
		t.Helper()
		var out struct {
			T []struct {
				Points *float64 `json:"task_estimate_points"`
			} `json:"t"`
		}
		if err := json.Unmarshal(dg.Query(t, fmt.Sprintf(`{ t(func: eq(task_uuid, %q)) { task_uuid task_estimate_points } }`, task)), &out); err != nil || len(out.T) != 1 {
			t.Fatalf("read %s: %v %+v", task, err, out)
		}
		return out.T[0].Points
	}
	five := 5.0
	if p := pointsOf(write("Ship", &five)); p == nil || *p != 5 {
		t.Fatalf("an issue estimated L (5) has %v points", p)
	}
	if p := pointsOf(write("Plan", nil)); p != nil {
		t.Fatalf("an issue with no estimate has %v points", *p)
	}

	// A team that doesn't estimate leaves its project on hours; a scale that
	// can't be kept does too, and the report says so once.
	keepEstimateScale(ctx, job, &importProvider.SourceProject{SourceID: "plain"}, plain, by)
	keepEstimateScale(ctx, job, &importProvider.SourceProject{SourceID: "odd", EstimateScale: &importProvider.SourceEstimateScale{Kind: "days"}}, plain, by)
	if got, err := estimateScale.Of(ctx, plain); err != nil || got != nil {
		t.Fatalf("Plain estimates in hours: %+v %v", got, err)
	}
	rows, err := importModels.ListErrors(ctx, job.Id, importModels.SeverityWarning, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("one warning, for the scale not kept: %v", rows)
	}
	if code, _ := rows[0]["code"].(*string); code == nil || *code != "ESTIMATE_SCALE_NOT_KEPT" {
		t.Fatalf("the warning: %v", rows[0])
	}
}
