package business

// A project's rules (package 9 R1). The project's routes check the caller's
// standing (controllers/ProjectAccess: NeedView to read, NeedManage to keep
// them) before these run; these check the rest: the rule belongs to the
// project, it is never set through a token, and it acts as whoever saved it
// last (who therefore could do every action in it by hand).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// ErrThroughToken is a rule set through an API or MCP token: rules act on
// their own for as long as they are on, so they are set in the app.
var ErrThroughToken = errors.New("rules are set in the app, not through a token")

// ErrTooManyRules is a project at its cap.
var ErrTooManyRules = fmt.Errorf("a project has at most %d rules", workflowModel.MaxRulesPerProject)

// errNotThisProject is a rule from another project (or a workspace one).
var errNotThisProject = errors.New("that rule isn't in this project")

// errProjectRule is a project's rule asked for through the workspace's
// workflow routes, which don't keep its conditions.
var errProjectRule = errors.New("that rule belongs to a project: change it there")

// Seams for tests.
var (
	setOwner    = workflowModel.SetOwner
	countRules  = workflowModel.CountByProject
	listRules   = workflowModel.ListByProject
	getRule     = workflowModel.GetWorkflowByID
	createRule  = workflowModel.CreateWorkflow
	updateRule  = workflowModel.UpdateWorkflow
	setRuleOn   = workflowModel.SetWorkflowActive
	deleteRule  = workflowModel.SoftDeleteWorkflow
	reloadRules = reload
)

// prepareProjectRule fills a project rule's input and checks it against the
// project.
func prepareProjectRule(ctx context.Context, projectID uuid.UUID, in *WorkflowInput) (*validatedWorkflow, error) {
	if helpers.ActsThroughToken(ctx) {
		return nil, ErrThroughToken
	}
	in.ProjectID = projectID.String()
	in.ChannelID = strings.TrimSpace(in.ChannelID)
	if in.TriggerConfig == nil {
		in.TriggerConfig = map[string]interface{}{}
	}
	// The zone of whoever saved it, kept with every rule (zone.go): where its
	// due dates are days and its due date steps land.
	tz, _ := in.TriggerConfig["tz"].(string)
	if _, err := zoneNamed(tz); err != nil {
		return nil, err
	}
	switch in.TriggerType {
	case workflowModel.TriggerTaskStatusChanged:
		in.TriggerConfig["project_id"] = projectID.String()
		if err := normalizeTaskStatusTrigger(ctx, in); err != nil {
			return nil, err
		}
	case workflowModel.TriggerTaskFieldChanged:
		if err := normalizeFields(in); err != nil {
			return nil, err
		}
	default:
		in.TriggerConfig = map[string]interface{}{}
	}
	if tz = strings.TrimSpace(tz); tz != "" {
		in.TriggerConfig["tz"] = tz
	}
	v, err := validate(in)
	if err != nil {
		return nil, err
	}
	if err := checkTaskActionsIn(ctx, projectID.String(), in.Actions, in.ElseActions); err != nil {
		return nil, err
	}
	// Statuses resolved to their ids: store what checkTaskActionsIn settled.
	if v2, err := validate(in); err == nil {
		v = v2
	}
	for _, c := range in.Conditions.Items {
		if (c.Field == "assignee" || c.Field == "creator") && c.Value != "" {
			if ok, err := memberOf(ctx, c.Value, projectID.String()); err != nil || !ok {
				return nil, fmt.Errorf("everyone a rule names must be in the project")
			}
		}
	}
	return v, nil
}

// normalizeFields keeps a field trigger's fields to the ones task.changed names.
func normalizeFields(in *WorkflowInput) error {
	raw, _ := in.TriggerConfig["fields"].([]interface{})
	var fields []string
	for _, f := range raw {
		s, _ := f.(string)
		switch s {
		case "priority", "due", "tags", "name":
			fields = append(fields, s)
		default:
			return fmt.Errorf("a rule can watch priority, due date, tags or name")
		}
	}
	in.TriggerConfig = map[string]interface{}{}
	if len(fields) > 0 {
		in.TriggerConfig["fields"] = fields
	}
	return nil
}

// ruleIn reads a rule and checks it is the project's.
func ruleIn(ctx context.Context, projectID, id uuid.UUID) (*workflowModel.Workflow, error) {
	wf, err := getRule(ctx, id)
	if err != nil {
		return nil, err
	}
	if wf == nil || wf.ProjectId == nil || *wf.ProjectId != projectID {
		return nil, errNotThisProject
	}
	return wf, nil
}

// CreateProjectRule saves a new rule in a project, acting as who.
func CreateProjectRule(ctx context.Context, who *userModels.UserInfo, projectID uuid.UUID, in WorkflowInput) (*workflowModel.Workflow, error) {
	v, err := prepareProjectRule(ctx, projectID, &in)
	if err != nil {
		return nil, err
	}
	n, err := countRules(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if n >= workflowModel.MaxRulesPerProject {
		return nil, ErrTooManyRules
	}
	id, err := createRule(ctx, who.UserPostgresInfo.Id, v.fields(&in))
	if err != nil {
		return nil, err
	}
	_ = reloadRules(ctx)
	return getRule(ctx, id)
}

// UpdateProjectRule saves a project's rule; whoever saves it becomes the one
// it acts as.
func UpdateProjectRule(ctx context.Context, who *userModels.UserInfo, projectID, id uuid.UUID, in WorkflowInput) (*workflowModel.Workflow, error) {
	if _, err := ruleIn(ctx, projectID, id); err != nil {
		return nil, err
	}
	v, err := prepareProjectRule(ctx, projectID, &in)
	if err != nil {
		return nil, err
	}
	if err := updateRule(ctx, id, v.fields(&in)); err != nil {
		return nil, err
	}
	if err := setOwner(ctx, id, who.UserPostgresInfo.Id); err != nil {
		return nil, err
	}
	_ = reloadRules(ctx)
	return getRule(ctx, id)
}

// SetProjectRuleActive turns a project's rule on or off. Turning it on makes
// who the one it acts as, and clears why it stopped.
func SetProjectRuleActive(ctx context.Context, who *userModels.UserInfo, projectID, id uuid.UUID, on bool) error {
	if helpers.ActsThroughToken(ctx) {
		return ErrThroughToken
	}
	if _, err := ruleIn(ctx, projectID, id); err != nil {
		return err
	}
	if err := setRuleOn(ctx, id, on); err != nil {
		return err
	}
	if on {
		if err := setOwner(ctx, id, who.UserPostgresInfo.Id); err != nil {
			return err
		}
	}
	return reloadRules(ctx)
}

// DeleteProjectRule deletes a project's rule (its history goes with it).
func DeleteProjectRule(ctx context.Context, projectID, id uuid.UUID) error {
	if helpers.ActsThroughToken(ctx) {
		return ErrThroughToken
	}
	if _, err := ruleIn(ctx, projectID, id); err != nil {
		return err
	}
	if err := deleteRule(ctx, id); err != nil {
		return err
	}
	return reloadRules(ctx)
}

// ListProjectRules is a project's rules.
func ListProjectRules(ctx context.Context, projectID uuid.UUID) ([]*workflowModel.Workflow, error) {
	rules, err := listRules(ctx, projectID)
	if rules == nil {
		rules = []*workflowModel.Workflow{}
	}
	return rules, err
}

// ListProjectRuns is a project's runs (one rule's when ruleID is set),
// newest first, before a time.
func ListProjectRuns(ctx context.Context, projectID uuid.UUID, ruleID *uuid.UUID, before *time.Time, limit int) ([]*workflowModel.Run, error) {
	if ruleID != nil {
		if _, err := ruleIn(ctx, projectID, *ruleID); err != nil {
			return nil, err
		}
	}
	return workflowModel.ListRuns(ctx, &projectID, ruleID, before, limit)
}

// IsNotThisProject says whether err is a rule that isn't the project's.
func IsNotThisProject(err error) bool { return errors.Is(err, errNotThisProject) }
