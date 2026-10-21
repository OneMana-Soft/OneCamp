package business

// The inbox's reading of the notices rules send (business/Follow
// noticeKinds.go): a task's, titled and opening the task only for a reader who
// may open it now (Work Access's task kind), as a doc's is; a rule's (one that
// stopped itself), titled with the rule's name for the person it acts as.

import (
	"context"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	followBusiness "github.com/akashc777/OneCamp/business/Follow"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

func init() {
	followBusiness.RegisterNoticeKind(workAccess.KindTask, taskNotices)
	followBusiness.RegisterNoticeKind("rule", ruleNotices)
}

// Seams for tests.
var (
	noticeAccount   = userDomain.GetActiveUserWithAdminFlagByUserUUID
	noticeDecideAll = accessBusiness.ResolveAll
	noticeTaskNames = taskDomain.TaskNames
)

func taskNotices(ctx context.Context, readerUUID string, ids []string) (map[string]bool, map[string]string, error) {
	reader, err := uuid.Parse(readerUUID)
	if err != nil {
		return nil, nil, nil
	}
	account, err := noticeAccount(ctx, reader)
	if err != nil {
		return nil, nil, err
	}
	readable, titles := map[string]bool{}, map[string]string{}
	if account == nil {
		return readable, titles, nil
	}
	res := make([]accessBusiness.Resource, 0, len(ids))
	for _, id := range ids {
		if _, err := uuid.Parse(id); err == nil {
			res = append(res, accessBusiness.Resource{Kind: workAccess.KindTask, UUID: strings.ToLower(id)})
		}
	}
	decided, err := noticeDecideAll(ctx, accessBusiness.Subject{UserUUID: readerUUID, Admin: account.IsAdmin}, res)
	if err != nil {
		return nil, nil, err
	}
	var open []string
	for r, d := range decided {
		if !d.Deleted && d.Level >= accessBusiness.LevelView {
			readable[r.UUID] = true
			open = append(open, r.UUID)
		}
	}
	// Read as the reader, so a restricted task's name is theirs only if they
	// may see it (models/taskread).
	names, err := noticeTaskNames(taskread.ActingFor(ctx, readerUUID), open)
	if err != nil {
		return nil, nil, err
	}
	for id, name := range names {
		if readable[id] {
			titles[id] = name
		}
	}
	return readable, titles, nil
}

// ruleNotices: a rule's notice is its owner's; it opens while the rule is
// there and is still theirs.
func ruleNotices(ctx context.Context, readerUUID string, ids []string) (map[string]bool, map[string]string, error) {
	readable, titles := map[string]bool{}, map[string]string{}
	for i, id := range ids {
		if i >= 50 {
			break
		}
		rid, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		w, err := getRule(ctx, rid)
		if err != nil || w == nil || !strings.EqualFold(w.CreatedBy.String(), readerUUID) {
			continue
		}
		key := strings.ToLower(id)
		readable[key] = true
		titles[key] = w.Name
	}
	return readable, titles, nil
}
