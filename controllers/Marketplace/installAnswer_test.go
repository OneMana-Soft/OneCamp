package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	business "github.com/akashc777/OneCamp/business/Marketplace"
	workflowBusiness "github.com/akashc777/OneCamp/business/Workflow"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// install answers one install request whose business.Install fails with err.
func install(t *testing.T, err error) *httptest.ResponseRecorder {
	t.Helper()
	old := installTemplate
	t.Cleanup(func() { installTemplate = old })
	installTemplate = func(context.Context, uuid.UUID, *userModels.UserInfo, business.InstallOptions) (*business.InstallResult, error) {
		return nil, err
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", uuid.NewString())
	req := httptest.NewRequest(http.MethodPost, "/marketplace/templates/x/install", strings.NewReader(`{"project_team_uuid":"`+uuid.NewString()+`"}`))
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	InstallTemplate(w, req)
	return w
}

// CR-444 F2: an install that fails on our side (a store's error, wrapped by
// the around: "couldn't make the project: <driver text>") is a 500 in words;
// the store's own text (constraint, table) is logged, never in the body.
func TestAnInstallThatFailsOnOurSideIsA500InWords(t *testing.T) {
	store := errors.New(`pq: insert or update on table "project_links" violates foreign key constraint "project_links_project_fk"`)
	for _, err := range []error{
		fmt.Errorf("couldn't make the project: %w", store),
		fmt.Errorf("couldn't read the team: %w", errors.New("rpc error: code = Unavailable desc = dgraph-alpha:9080")),
		errors.New("failed to load template"),
	} {
		w := install(t, err)
		body := w.Body.String()
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%v: status %d, want 500", err, w.Code)
		}
		if !strings.Contains(body, "Couldn't install the template. Please try again.") {
			t.Errorf("%v: body %s has no words", err, body)
		}
		for _, leak := range []string{"pq:", "project_links", "constraint", "dgraph", "couldn't make", "couldn't read", "failed to load"} {
			if strings.Contains(body, leak) {
				t.Errorf("%v: body %s carries %q", err, body, leak)
			}
		}
	}
}

// A refusal keeps its status and its words, wrapped or not: a name taken just
// now is a 409, and a team you may not make projects in a 403.
func TestARefusedInstallKeepsItsWords(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		words  string
	}{
		{&business.Refusal{Status: http.StatusConflict, Msg: "A project or channel by that name was made just now. Try again."}, http.StatusConflict, "made just now"},
		{fmt.Errorf("around: %w", &business.Refusal{Status: http.StatusForbidden, Msg: "Only a team's admins can make projects in it."}), http.StatusForbidden, "Only a team's admins"},
	} {
		w := install(t, c.err)
		if w.Code != c.status || !strings.Contains(w.Body.String(), c.words) {
			t.Errorf("%v: got %d %s, want %d with %q", c.err, w.Code, w.Body.String(), c.status, c.words)
		}
	}
}

// CR-444 D1: what a kind's own create refuses is the person's to change, not
// ours to retry, so an install answers it in its own status and words (Phase A
// did), not 500 "Please try again"; what fails on our side under the same
// wrap is still the 500 in words. Wrapped the way Install wraps it: made by
// the real CreateWorkflow on paths that read no store, a task-status workflow
// whose reply has no channel (an install leaves its channel unbound, so such a
// template refuses every time) and what its validate refuses (no name, a
// trigger it doesn't know: CR-459 D2); a rule set through a token
// (403); and any kind's helpers.Refusal, as an agent's create makes them. That
// an agent's create does is proven in business/AIAgent's own tests
// (TestWhatAnAgentsCreateRefusesAnInstallReadsInItsWords): this package may
// not import AI code, since the AI-free edition builds without it.
func TestAnInstallItsKindRefusesKeepsItsWords(t *testing.T) {
	_, flowErr := workflowBusiness.CreateWorkflow(context.Background(), workflowBusiness.WorkflowInput{
		Name:        "Tell the channel",
		TriggerType: workflowModel.TriggerTaskStatusChanged,
		Actions:     []workflowBusiness.WorkflowAction{{Type: workflowBusiness.ActionReply}},
	}, uuid.New())
	_, unnamedErr := workflowBusiness.CreateWorkflow(context.Background(), workflowBusiness.WorkflowInput{}, uuid.New())
	_, triggerErr := workflowBusiness.CreateWorkflow(context.Background(), workflowBusiness.WorkflowInput{Name: "Odd", TriggerType: "no.such.trigger"}, uuid.New())
	const ours = "Couldn't install the template. Please try again."
	for _, c := range []struct {
		made   error
		wrap   string
		status int
		words  string
	}{
		{flowErr, "failed to install workflow: %w", http.StatusBadRequest, "choose the channel its reply posts in"},
		{unnamedErr, "failed to install workflow: %w", http.StatusBadRequest, "name is required"},
		{triggerErr, "failed to install workflow: %w", http.StatusBadRequest, "unsupported trigger type"},
		{workflowBusiness.ErrThroughToken, "failed to install workflow: %w", http.StatusForbidden, "rules are set in the app, not through a token"},
		{&helpers.Refusal{Status: http.StatusBadRequest, Msg: "invalid model selection"}, "failed to install agent: %w", http.StatusBadRequest, "invalid model selection"},
		{errors.New("failed to create agent"), "failed to install agent: %w", http.StatusInternalServerError, ours},
	} {
		if c.made == nil || c.words != ours && !strings.Contains(c.made.Error(), c.words) {
			t.Fatalf("the create answered %v, want %q", c.made, c.words)
		}
		w := install(t, fmt.Errorf(c.wrap, c.made))
		body := w.Body.String()
		if w.Code != c.status || !strings.Contains(body, c.words) || strings.Contains(body, "failed to") {
			t.Errorf("%q: got %d %s, want %d with only its words", c.words, w.Code, body, c.status)
		}
	}
}

// V-1281: a kind's check that couldn't read the graph (an agent's checkReach,
// a workflow's checkScope: helpers.RefusedUnless) is ours, not the person's,
// so an install answers it 500 in our words, never 400 "choose ...", and the
// cause stays in the log. What the check refused, or found nothing by, keeps
// the 400 in its words.
func TestAnInstallWhoseCheckCouldntReadIsA500(t *testing.T) {
	ctx := context.Background()
	choose := &helpers.Refusal{Status: http.StatusBadRequest, Msg: "choose a channel the workflow's owner can read"}
	unreachable := errors.New("rpc error: code = Unavailable desc = dgraph-alpha:9080")
	const ours = "Couldn't install the template. Please try again."
	for _, c := range []struct {
		name   string
		made   error
		status int
		words  string
	}{
		{"a read that failed", helpers.RefusedUnless(ctx, "test checkScope", false, unreachable, choose), http.StatusInternalServerError, ours},
		{"a no", helpers.RefusedUnless(ctx, "test checkScope", false, nil, choose), http.StatusBadRequest, choose.Msg},
		{"nothing by that id", helpers.RefusedUnless(ctx, "test checkScope", false, helpers.NotFound("channel not found in dgraph"), choose), http.StatusBadRequest, choose.Msg},
	} {
		w := install(t, fmt.Errorf("failed to install workflow: %w", c.made))
		body := w.Body.String()
		if w.Code != c.status || !strings.Contains(body, c.words) {
			t.Errorf("%s: got %d %s, want %d with %q", c.name, w.Code, body, c.status, c.words)
		}
		for _, leak := range []string{"dgraph", "rpc error", "failed to", "not found"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s: body %s carries %q", c.name, body, leak)
			}
		}
	}
}
