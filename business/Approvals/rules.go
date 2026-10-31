package business

// Keeping a project's status rules. The project's routes check the caller
// may (members read, admins change); these check the rule: a status the
// project has, people who are in it, approvals it can collect.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	"github.com/google/uuid"
)

// MaxPeople bounds the people a rule names.
const MaxPeople = 20

// RuleInput is a status's rule as it is set.
type RuleInput struct {
	Status          string   `json:"status"`
	Who             string   `json:"who"`
	People          []string `json:"people"`
	Required        []string `json:"required"`
	Approvers       []string `json:"approvers"`
	ApprovalsNeeded int      `json:"approvals_needed"`
	// Client: the project's clients approve too, through its comment links.
	Client bool `json:"client"`
}

var (
	membersAmong = projectBusiness.MembersAmong
	putRule      = model.PutRule
	countRules   = model.CountRules
	listRules    = model.ListRules
	dropRule     = model.DeleteRule
	resolve      = taskStatusBusiness.Resolve
)

// ErrTooMany is a project at its cap.
var ErrTooMany = fmt.Errorf("a project has at most %d status rules", model.MaxRulesPerProject)

func people(ctx context.Context, projectID uuid.UUID, ids []string, what string) ([]string, error) {
	if len(ids) > MaxPeople {
		return nil, fmt.Errorf("at most %d %s", MaxPeople, what)
	}
	var out []string
	for _, raw := range ids {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("pick %s from the project", what)
		}
		if !slices.Contains(out, id.String()) {
			out = append(out, id.String())
		}
	}
	// Who is in the project, read once for all of them.
	in, err := membersAmong(ctx, projectID.String(), out)
	if err != nil {
		return nil, err
	}
	for _, id := range out {
		if !in[id] {
			return nil, fmt.Errorf("everyone a status rule names must be in the project")
		}
	}
	return out, nil
}

// SaveRule sets a status's rule; a rule that asks nothing is removed.
func SaveRule(ctx context.Context, by uuid.UUID, projectID uuid.UUID, in RuleInput) (*model.StatusRule, error) {
	if helpers.ActsThroughToken(ctx) {
		return nil, errors.New("status rules are set in the app, not through a token")
	}
	r, err := resolve(ctx, projectID.String(), in.Status)
	if errors.Is(err, taskStatusBusiness.ErrUnknownStatus) {
		return nil, fmt.Errorf("%q is not a status of this project", in.Status)
	}
	if err != nil {
		return nil, err
	}
	rule := &model.StatusRule{ProjectID: projectID, Status: r.Value(), Who: in.Who, UpdatedBy: by, ApprovalsNeeded: in.ApprovalsNeeded, Client: in.Client}
	switch rule.Who {
	case "", "anyone":
		rule.Who, rule.People = "anyone", []string{}
	case "admins":
		rule.People = []string{}
	case "people":
		if rule.People, err = people(ctx, projectID, in.People, "people"); err != nil {
			return nil, err
		}
		if len(rule.People) == 0 {
			return nil, fmt.Errorf("pick who may move tasks there")
		}
	default:
		return nil, fmt.Errorf("say anyone, the project's admins, or the people you pick")
	}
	rule.Required = []string{}
	for _, f := range in.Required {
		if _, ok := requirable[f]; !ok {
			return nil, fmt.Errorf("a status can require an assignee, a due date, a tag or a description")
		}
		if !slices.Contains(rule.Required, f) {
			rule.Required = append(rule.Required, f)
		}
	}
	if rule.Approvers, err = people(ctx, projectID, in.Approvers, "approvers"); err != nil {
		return nil, err
	}
	if rule.Approvers == nil {
		rule.Approvers = []string{}
	}
	if len(rule.Approvers) == 0 {
		rule.ApprovalsNeeded = 1
	} else if rule.ApprovalsNeeded < 1 || rule.ApprovalsNeeded > len(rule.Approvers) {
		return nil, fmt.Errorf("approvals needed is 1 to %d", len(rule.Approvers))
	}
	if rule.Who == "anyone" && len(rule.Required) == 0 && !rule.WantsApproval() {
		return nil, dropRule(ctx, projectID, rule.Status)
	}
	existing, err := listRules(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(existing, func(x *model.StatusRule) bool { return x.Status == rule.Status }) {
		if n, err := countRules(ctx, projectID); err != nil {
			return nil, err
		} else if n >= model.MaxRulesPerProject {
			return nil, ErrTooMany
		}
	}
	if err := putRule(ctx, rule); err != nil {
		return nil, err
	}
	return rule, nil
}

// ListRules is a project's status rules.
func ListRules(ctx context.Context, projectID uuid.UUID) ([]*model.StatusRule, error) {
	return listRules(ctx, projectID)
}

// DeleteRule removes a status's rule.
func DeleteRule(ctx context.Context, projectID uuid.UUID, status string) error {
	if helpers.ActsThroughToken(ctx) {
		return errors.New("status rules are set in the app, not through a token")
	}
	return dropRule(ctx, projectID, strings.TrimSpace(status))
}
