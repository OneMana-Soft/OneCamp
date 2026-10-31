package business

import (
	"context"
	"testing"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	"github.com/google/uuid"
)

// ruleStore keeps rules the way SaveRule's seams see them.
type ruleStore struct {
	put     []*model.StatusRule
	dropped []string
}

func (s *ruleStore) use(t *testing.T) {
	oldR, oldM, oldL, oldC, oldP, oldD := resolve, membersAmong, listRules, countRules, putRule, dropRule
	t.Cleanup(func() {
		resolve, membersAmong, listRules, countRules, putRule, dropRule = oldR, oldM, oldL, oldC, oldP, oldD
	})
	resolve = func(_ context.Context, _ string, status string) (taskStatusBusiness.Resolved, error) {
		return taskStatusBusiness.Resolved{Category: status}, nil
	}
	membersAmong = func(_ context.Context, _ string, ids []string) (map[string]bool, error) {
		in := map[string]bool{}
		for _, id := range ids {
			in[id] = true
		}
		return in, nil
	}
	listRules = func(context.Context, uuid.UUID) ([]*model.StatusRule, error) { return s.put, nil }
	countRules = func(context.Context, uuid.UUID) (int, error) { return len(s.put), nil }
	putRule = func(_ context.Context, r *model.StatusRule) error {
		s.put = append(s.put, r)
		return nil
	}
	dropRule = func(_ context.Context, _ uuid.UUID, status string) error {
		s.dropped = append(s.dropped, status)
		return nil
	}
}

// A status that wants only the client's approval asks something, so its
// rule is kept; a rule that asks nothing at all is removed.
func TestAClientOnlyRuleIsKept(t *testing.T) {
	s := &ruleStore{}
	s.use(t)
	ctx, by, project := context.Background(), uuid.New(), uuid.New()

	r, err := SaveRule(ctx, by, project, RuleInput{Status: "done", Who: "anyone", Client: true})
	if err != nil || r == nil || !r.Client || len(s.put) != 1 || len(s.dropped) != 0 {
		t.Fatalf("a rule that wants the client: %+v %v; kept %d, removed %v", r, err, len(s.put), s.dropped)
	}
	if r, err := SaveRule(ctx, by, project, RuleInput{Status: "done", Who: "anyone"}); err != nil || r != nil || len(s.dropped) != 1 {
		t.Errorf("a rule that asks nothing: %+v %v; removed %v", r, err, s.dropped)
	}
}
