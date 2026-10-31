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
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
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
	// formIn says whether a form is the project's and makes tasks: a form
	// rule's form (one that adds table rows has no task to run on).
	formIn = formModel.MakesTasks
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
	// due dates are days, its due date steps land and its schedule keeps time.
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
	case workflowModel.TriggerSchedule:
		sch, err := parseSchedule(in.TriggerConfig, clock())
		if err != nil {
			return nil, err
		}
		in.TriggerConfig = map[string]interface{}{"repeat": sch.cfg.Repeat, "at": sch.cfg.At, "from": sch.cfg.From}
		if sch.cfg.EachTask {
			in.TriggerConfig["each_task"] = true
		}
		for i := range in.Actions {
			if in.Actions[i].Type == ActionCreateTask {
				in.Actions[i].ProjectID = projectID.String()
			}
		}
	case workflowModel.TriggerFormSubmitted:
		// One of this project's forms: its id is the client's, so it is
		// read back here, never taken on trust.
		raw, _ := in.TriggerConfig["form_id"].(string)
		form, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("pick the form the rule runs on")
		}
		ok, err := formIn(ctx, projectID, form)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("pick one of this project's forms that make tasks")
		}
		in.TriggerConfig = map[string]interface{}{"form_id": form.String()}
	case workflowModel.TriggerApprovalDecided:
		// How the request must end; "any" (or none) is every ending.
		o, _ := in.TriggerConfig["outcome"].(string)
		switch o = strings.TrimSpace(o); o {
		case "", "any":
			in.TriggerConfig = map[string]interface{}{}
		case "approved", "changes", "rejected":
			in.TriggerConfig = map[string]interface{}{"outcome": o}
		default:
			return nil, fmt.Errorf("say how the approval must end: approved, changes asked for, or rejected")
		}
	case workflowModel.TriggerTaskDueSoon, workflowModel.TriggerTaskOverdue:
		d, err := parseDue(in.TriggerType, in.TriggerConfig)
		if err != nil {
			return nil, err
		}
		in.TriggerConfig = map[string]interface{}{"at": d.cfg.At}
		if in.TriggerType == workflowModel.TriggerTaskDueSoon {
			in.TriggerConfig["days"] = d.cfg.Days
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
	if in.TriggerType == workflowModel.TriggerSchedule {
		if err := syncSchedule(ctx, id); err != nil {
			helpers.LogErrorWithContext(ctx, "Workflow %s plan schedule: %v", id, err)
		}
	}
	return getRule(ctx, id)
}

// UpdateProjectRule saves a project's rule; whoever saves it becomes the one
// it acts as.
func UpdateProjectRule(ctx context.Context, who *userModels.UserInfo, projectID, id uuid.UUID, in WorkflowInput) (*workflowModel.Workflow, error) {
	was, err := ruleIn(ctx, projectID, id)
	if err != nil {
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
	// Its job moves with it, and goes when it stops being a schedule.
	if was.TriggerType == workflowModel.TriggerSchedule || in.TriggerType == workflowModel.TriggerSchedule {
		if err := syncSchedule(ctx, id); err != nil {
			helpers.LogErrorWithContext(ctx, "Workflow %s plan schedule: %v", id, err)
		}
	}
	return getRule(ctx, id)
}

// SetProjectRuleActive turns a project's rule on or off. Turning it on makes
// who the one it acts as, and clears why it stopped.
func SetProjectRuleActive(ctx context.Context, who *userModels.UserInfo, projectID, id uuid.UUID, on bool) error {
	if helpers.ActsThroughToken(ctx) {
		return ErrThroughToken
	}
	wf, err := ruleIn(ctx, projectID, id)
	if err != nil {
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
	if wf.TriggerType == workflowModel.TriggerSchedule {
		if err := syncSchedule(ctx, id); err != nil {
			helpers.LogErrorWithContext(ctx, "Workflow %s plan schedule: %v", id, err)
		}
	}
	return reloadRules(ctx)
}

// DeleteProjectRule deletes a project's rule (its history goes with it).
func DeleteProjectRule(ctx context.Context, projectID, id uuid.UUID) error {
	if helpers.ActsThroughToken(ctx) {
		return ErrThroughToken
	}
	wf, err := ruleIn(ctx, projectID, id)
	if err != nil {
		return err
	}
	if err := deleteRule(ctx, id); err != nil {
		return err
	}
	if wf.TriggerType == workflowModel.TriggerSchedule {
		if err := cancelScheduleJob(ctx, id); err != nil {
			helpers.LogErrorWithContext(ctx, "Workflow %s cancel schedule: %v", id, err)
		}
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
