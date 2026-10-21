package business

import (
	"context"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// A rule's notice about a task is titled, and opens, only for a reader who
// may open the task now; a rule's own notice only for the person it acts as.
func TestRuleNoticesNameOnlyWhatTheirReaderMayOpen(t *testing.T) {
	oldA, oldD, oldN, oldG := noticeAccount, noticeDecideAll, noticeTaskNames, getRule
	t.Cleanup(func() { noticeAccount, noticeDecideAll, noticeTaskNames, getRule = oldA, oldD, oldN, oldG })
	open, closed := uuid.NewString(), uuid.NewString()
	noticeAccount = func(context.Context, uuid.UUID) (*userModel.User, error) { return &userModel.User{}, nil }
	noticeDecideAll = func(_ context.Context, _ accessBusiness.Subject, res []accessBusiness.Resource) (map[accessBusiness.Resource]accessBusiness.Decision, error) {
		out := map[accessBusiness.Resource]accessBusiness.Decision{}
		for _, r := range res {
			lvl := accessBusiness.LevelNone
			if r.UUID == open {
				lvl = accessBusiness.LevelView
			}
			out[r] = accessBusiness.Decision{Level: lvl}
		}
		return out, nil
	}
	noticeTaskNames = func(_ context.Context, ids []string) (map[string]string, error) {
		out := map[string]string{}
		for _, id := range ids {
			out[id] = "Fix SSO"
		}
		return out, nil
	}
	readable, titles, err := taskNotices(context.Background(), uuid.NewString(), []string{open, closed})
	if err != nil || !readable[open] || readable[closed] || titles[open] != "Fix SSO" || titles[closed] != "" {
		t.Fatalf("readable %v titles %v err %v", readable, titles, err)
	}

	owner, rule := uuid.New(), uuid.New()
	getRule = func(context.Context, uuid.UUID) (*workflowModel.Workflow, error) {
		return &workflowModel.Workflow{Id: rule, Name: "Triage", CreatedBy: owner}, nil
	}
	if r, ti, _ := ruleNotices(context.Background(), owner.String(), []string{rule.String()}); !r[rule.String()] || ti[rule.String()] != "Triage" {
		t.Errorf("the owner: %v %v", r, ti)
	}
	if r, _, _ := ruleNotices(context.Background(), uuid.NewString(), []string{rule.String()}); r[rule.String()] {
		t.Error("someone else opens a rule's notice")
	}
}
