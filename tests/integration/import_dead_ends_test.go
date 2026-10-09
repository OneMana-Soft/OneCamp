//go:build integration
// +build integration

package integration_test

// The import's dead ends: a token is tested when Connect is pressed and its
// refusal said in words (and not saved), the workspace list says why it
// couldn't load, a failed import can be planned again, an import waiting to
// be planned or run can be discarded (freeing its label), and one left
// waiting for a day is set aside.
//
// Run: go test -tags=integration ./tests/integration/ -run TestImportDeadEnds -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	importBusiness "github.com/akashc777/OneCamp/business/Import"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	_ "github.com/akashc777/OneCamp/business/Import/providers"
	importController "github.com/akashc777/OneCamp/controllers/Import"
	slackImportController "github.com/akashc777/OneCamp/controllers/SlackImport"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// stubProvider is an import source whose checks answer what the test says.
type stubProvider struct {
	importProvider.Provider
	validateErr error
}

func (s *stubProvider) Name() string                            { return "stubsource" }
func (s *stubProvider) Capabilities() importProvider.Capability { return importProvider.CapProjects }
func (s *stubProvider) Validate(context.Context, *importModels.Job, importProvider.JobOptions) error {
	return s.validateErr
}
func (s *stubProvider) Plan(context.Context, *importModels.Job, importProvider.JobOptions) (*importProvider.Plan, []*importModels.Chunk, error) {
	return &importProvider.Plan{ProjectCount: 2}, nil, nil
}
func (s *stubProvider) CleanupJob(string) {}

func TestImportDeadEnds(t *testing.T) {
	ctx := context.Background()
	t.Setenv("IMPORT_TOKEN_KEK", "integration-kek-1234567890abcdefghij")
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	stub := &stubProvider{}
	importProvider.Register(stub)

	admin := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, 'admin@acme.test')`, admin); err != nil {
		t.Fatal(err)
	}
	asAdmin := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: admin, IsAdmin: true},
	})
	call := func(h http.HandlerFunc, params map[string]string, body any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
		rc := chi.NewRouteContext()
		for k, v := range params {
			rc.URLParams.Add(k, v)
		}
		req = req.WithContext(context.WithValue(asAdmin, chi.RouteCtxKey, rc))
		rec := httptest.NewRecorder()
		h(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	job := func(provider, label, status, source string, updated time.Time) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO import_jobs (id, provider, source_workspace_name, source, status, triggered_by, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, provider, label, source, status, admin, updated); err != nil {
			t.Fatal(err)
		}
		return id
	}
	statusOf := func(id uuid.UUID) (status, msg string) {
		t.Helper()
		var m *string
		if err := env.PG.QueryRow(`SELECT status, error_message FROM import_jobs WHERE id = $1`, id).Scan(&status, &m); err != nil {
			t.Fatal(err)
		}
		if m != nil {
			msg = *m
		}
		return status, msg
	}

	// 1. Connect tests the token first. Jira at a site that refuses it: said
	// in words, with the classic-token advice, and nothing saved.
	var refuse atomic.Bool
	refuse.Store(true)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"isLast":true,"values":[{"id":"1","key":"ENG","name":"Engineering"}]}`)
	}))
	defer site.Close()
	connect := func(siteURL string) (int, map[string]any) {
		return call(importController.HandleConnect, map[string]string{"provider": "jira"}, map[string]any{
			"access_token": "atl-token", "metadata": map[string]string{"email": "admin@acme.test", "site_url": siteURL},
		})
	}
	if code, out := connect(site.URL); code != http.StatusBadRequest || out["code"] != "token_rejected" || !strings.Contains(fmt.Sprint(out["error"]), "classic API token") {
		t.Fatalf("a refused token: %d %v", code, out)
	}
	if _, err := importModels.LoadToken(ctx, "jira", admin); !errors.Is(err, importModels.ErrTokenNotFound) {
		t.Fatalf("a refused token was saved: %v", err)
	}
	// With nothing saved, the workspace list says to connect: "not_connected",
	// which the card answers with Reconnect.
	if code, out := call(importController.HandleDiscover, map[string]string{"provider": "jira"}, nil); code != http.StatusBadRequest || out["code"] != "not_connected" {
		t.Fatalf("the list with nothing connected: %d %v", code, out)
	}
	// A site nobody answers at is named.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + l.Addr().String()
	l.Close()
	if code, out := connect(dead); code != http.StatusServiceUnavailable || out["code"] != "unreachable" || !strings.Contains(fmt.Sprint(out["error"]), dead) {
		t.Fatalf("a site that can't be reached: %d %v", code, out)
	}
	// A good token is saved.
	refuse.Store(false)
	if code, out := connect(site.URL); code != http.StatusOK {
		t.Fatalf("a good token: %d %v", code, out)
	}
	if _, err := importModels.LoadToken(ctx, "jira", admin); err != nil {
		t.Fatalf("a good token wasn't saved: %v", err)
	}

	// 2. The workspace list says why it failed, with a code for Reconnect,
	// and never as a 401 (the web app would sign the admin out).
	refuse.Store(true)
	if code, out := call(importController.HandleDiscover, map[string]string{"provider": "jira"}, nil); code != http.StatusBadRequest || out["code"] != "token_rejected" {
		t.Fatalf("the list with a refused token: %d %v", code, out)
	}

	// 3. A failed import can be planned again: its connection is checked,
	// then it waits again and is planned.
	failed := job("stubsource", "Acme", "failed", "api", time.Now())
	stub.validateErr = &importProvider.TokenRejected{Msg: "stub auth failed"}
	if code, out := call(importController.HandlePlan, map[string]string{"jobId": failed.String()}, nil); code != http.StatusBadRequest || out["code"] != "token_rejected" {
		t.Fatalf("planning again with a refused token: %d %v", code, out)
	}
	if s, _ := statusOf(failed); s != "failed" {
		t.Fatalf("a refused plan-again left the job %s", s)
	}
	stub.validateErr = nil
	if code, out := call(importController.HandlePlan, map[string]string{"jobId": failed.String()}, nil); code != http.StatusOK || out["project_count"] != float64(2) {
		t.Fatalf("planning a failed import again: %d %v", code, out)
	}
	if s, msg := statusOf(failed); s != "planned" || msg != "" {
		t.Fatalf("planned again: %s %q", s, msg)
	}
	// Its label taken by another import in the meantime: said.
	again := job("stubsource", "Beta", "failed", "api", time.Now())
	job("stubsource", "Beta", "validating", "api", time.Now())
	if code, out := call(importController.HandlePlan, map[string]string{"jobId": again.String()}, nil); code != http.StatusConflict || out["code"] != "active_job" {
		t.Fatalf("planning again while another import holds the label: %d %v", code, out)
	}
	// An uploaded file that has been cleared away: said.
	gone := job("stubsource", "Gamma", "failed", "export_zip", time.Now())
	if code, out := call(importController.HandlePlan, map[string]string{"jobId": gone.String()}, nil); code != http.StatusConflict || out["code"] != "file_gone" {
		t.Fatalf("planning again with the upload gone: %d %v", code, out)
	}

	// The same for an uploaded Slack export.
	slackGone := job("slack", "Slack Gone", "failed", "export_zip", time.Now())
	if code, out := call(slackImportController.HandlePlan, map[string]string{"jobId": slackGone.String()}, nil); code != http.StatusConflict || out["code"] != "file_gone" {
		t.Fatalf("planning a Slack import again with the export gone: %d %v", code, out)
	}
	slackAgain := job("slack", "Slack Busy", "failed", "export_zip", time.Now())
	if _, err := env.PG.Exec(`UPDATE import_jobs SET raw_object_key = 'slackImport/x/raw.zip' WHERE id = $1`, slackAgain); err != nil {
		t.Fatal(err)
	}
	job("slack", "Slack Busy", "planned", "export_zip", time.Now())
	if code, out := call(slackImportController.HandlePlan, map[string]string{"jobId": slackAgain.String()}, nil); code != http.StatusConflict || out["code"] != "active_job" {
		t.Fatalf("planning a Slack import again while another holds the label: %d %v", code, out)
	}
	if s, _ := statusOf(slackAgain); s != "failed" {
		t.Fatalf("a refused Slack plan-again left the job %s", s)
	}

	// Running a failed import, or retrying its failed chunks, starts it: it
	// used to answer as if it had while the job stayed failed.
	rerun := job("stubsource", "Rerun", "failed", "api", time.Now())
	if started, err := importModels.StartRunning(ctx, rerun, []string{"planned", "failed"}); err != nil || !started {
		t.Fatalf("running a failed import: %v %v", started, err)
	}
	if s, _ := statusOf(rerun); s != "running" {
		t.Fatalf("a failed import run again is %s", s)
	}
	if started, err := importModels.StartRunning(ctx, rerun, []string{"planned", "failed"}); err != nil || started {
		t.Fatalf("a job already running doesn't start twice: %v %v", started, err)
	}
	busy := job("stubsource", "Busy", "cancelled", "api", time.Now())
	job("stubsource", "Busy", "planned", "api", time.Now())
	if _, err := importModels.StartRunning(ctx, busy, []string{"failed", "cancelled", "completed"}); !errors.Is(err, importModels.ErrConflictActiveJob) {
		t.Fatalf("retrying while another import holds the label: %v", err)
	}

	// 4. An import waiting to be planned or run can be discarded, and its
	// label is free again.
	for _, status := range []string{"pending", "validating", "planned"} {
		label := "Discard " + status
		waiting := job("stubsource", label, status, "api", time.Now())
		if code, out := call(importController.HandleCancel, map[string]string{"jobId": waiting.String()}, nil); code != http.StatusOK {
			t.Fatalf("discarding a %s import: %d %v", status, code, out)
		}
		if s, msg := statusOf(waiting); s != "cancelled" || msg != "Discarded before it ran." {
			t.Fatalf("discarded %s: %s %q", status, s, msg)
		}
		if err := importModels.CreateJob(ctx, &importModels.Job{Id: uuid.New(), Provider: "stubsource", SourceWorkspaceName: label, Source: "api", Status: "validating", TriggeredBy: &admin}); err != nil {
			t.Fatalf("the label of a discarded %s import is still taken: %v", status, err)
		}
	}
	slackWaiting := job("slack", "Acme Slack", "validating", "export_zip", time.Now())
	if code, out := call(slackImportController.HandleCancel, map[string]string{"jobId": slackWaiting.String()}, nil); code != http.StatusOK {
		t.Fatalf("discarding an uploaded Slack export: %d %v", code, out)
	}
	if s, _ := statusOf(slackWaiting); s != "cancelled" {
		t.Fatalf("a discarded Slack export is %s", s)
	}

	// 5. One left waiting for more than a day is set aside, freeing its label;
	// one touched an hour ago is not.
	stale := job("stubsource", "Stale", "planned", "api", time.Now().Add(-30*time.Hour))
	fresh := job("stubsource", "Fresh", "validating", "api", time.Now().Add(-time.Hour))
	if n, err := importBusiness.SetAsideAbandonedImports(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("set aside %d (%v), want 1", n, err)
	}
	if s, msg := statusOf(stale); s != "failed" || !strings.HasPrefix(msg, "Waited more than a day") {
		t.Fatalf("the stale import: %s %q", s, msg)
	}
	if s, _ := statusOf(fresh); s != "validating" {
		t.Fatalf("the fresh import was set aside: %s", s)
	}
	if err := importModels.CreateJob(ctx, &importModels.Job{Id: uuid.New(), Provider: "stubsource", SourceWorkspaceName: "Stale", Source: "api", Status: "validating", TriggeredBy: &admin}); err != nil {
		t.Fatalf("the stale import's label is still taken: %v", err)
	}
	// And it can be planned again.
	if code, out := call(importController.HandlePlan, map[string]string{"jobId": stale.String()}, nil); code != http.StatusConflict || out["code"] != "active_job" {
		t.Fatalf("planning the set-aside import while a new one holds its label: %d %v", code, out)
	}
}
