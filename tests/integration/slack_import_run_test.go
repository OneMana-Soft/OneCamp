//go:build integration
// +build integration

package integration_test

// A Slack import starts once. Run read the job's status and then wrote
// "running" over whatever it had become, so two clicks (or a retried request)
// started two runs of the same import: every channel made twice, and Cancel
// stopping only one of them. And a stage that writes "running" after the
// admin's cancel no longer carries the import on.
//
// Run: go test -tags=integration ./tests/integration/ -run TestASlackImportStartsOnce -v

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	slackBusiness "github.com/akashc777/OneCamp/business/SlackImport"
	slackController "github.com/akashc777/OneCamp/controllers/SlackImport"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	slackModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestASlackImportStartsOnce(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)

	admin, job, late := uuid.New(), uuid.New(), uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username) VALUES ($1, 'admin@acme.test', 'admin')`, admin); err != nil {
		t.Fatal(err)
	}
	// Planned, and (like any test job) with no export behind it: the one run
	// that starts fails at once, saying so.
	if _, err := env.PG.Exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status, triggered_by)
		VALUES ($1, 'slack', 'Acme', 'export_zip', 'planned', $3), ($2, 'slack', 'Beta', 'export_zip', 'planned', $3)`,
		job, late, admin); err != nil {
		t.Fatal(err)
	}
	asAdmin := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: admin, EmailID: "admin@acme.test", IsAdmin: true},
	})
	run := func() int {
		rc := chi.NewRouteContext()
		rc.URLParams.Add("jobId", job.String())
		rec := httptest.NewRecorder()
		slackController.HandleRun(rec, httptest.NewRequest(http.MethodPost, "/", nil).WithContext(context.WithValue(asAdmin, chi.RouteCtxKey, rc)))
		return rec.Code
	}

	// Five clicks at once (the run limit is five a window).
	codes := make([]int, 5)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = run()
		}(i)
	}
	close(start)
	wg.Wait()
	started := 0
	for _, code := range codes {
		switch code {
		case http.StatusAccepted:
			started++
		case http.StatusConflict:
		default:
			t.Errorf("a click answered %d", code)
		}
	}
	if started != 1 {
		t.Errorf("%d runs started from %v, want one", started, codes)
	}
	waitFor(t, "the run to end", func() bool {
		j, err := slackModels.GetJob(ctx, job)
		return err == nil && j.Status == slackModels.StatusFailed
	})

	// A late cancel: the admin cancels a running import, and a stage that was
	// already under way writes "running" afterwards.
	if ok, err := slackModels.StartRunning(ctx, late, []string{slackModels.StatusPlanned, slackModels.StatusFailed}); err != nil || !ok {
		t.Fatalf("start: %v %v", ok, err)
	}
	if err := slackBusiness.CancelImport(ctx, late); err != nil {
		t.Fatal(err)
	}
	stage := "messages"
	if err := slackModels.UpdateStatus(ctx, late, slackModels.StatusRunning, &stage, nil); err != nil {
		t.Fatal(err)
	}
	if j, err := slackModels.GetJob(ctx, late); err != nil || j.Status != slackModels.StatusCancelled {
		t.Errorf("after a late stage write: %+v %v, want cancelled", j, err)
	}
	if ok, err := slackModels.StartRunning(ctx, late, []string{slackModels.StatusPlanned, slackModels.StatusFailed}); err != nil || ok {
		t.Errorf("a cancelled import was started again: %v %v", ok, err)
	}
}
