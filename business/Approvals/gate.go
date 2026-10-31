// Package business (Approvals) is a project's status rules and approvals
// (package 9 R2; design/projects-vs-asana-jira/PROGRAM.md §16, B6's
// transition rules and B8). A status may say who moves a task into it, what
// the task must have first, and who must approve the move. The gate is asked
// by every status change (business/Task StatusGate), so the app, the API,
// boards, GitHub, agents and rules all keep it alike; a move that needs
// approving becomes a request, made as the person who asked once enough
// approvers say yes.
package business

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/Approvals"
	"github.com/google/uuid"
)

func init() {
	taskBusiness.StatusGate = gate
	taskBusiness.FirstStatuses = firstStatuses
	taskBusiness.WentOn = wentOn
}

// wentOn closes a task's open request when the task goes on without it
// (business/Task WentOn): the waiting lists drop it, and a late answer is
// told it's closed instead of pulling the task back. An ask about
// the task itself isn't about its status, so only a delete closes one.
func wentOn(ctx context.Context, taskID uuid.UUID, deleted bool) {
	if err := closeOpenFor(ctx, taskID, model.MovedOn, deleted); err != nil {
		helpers.LogErrorWithContext(ctx, "Approvals: closing the open request of %s, which went on without it: %v", taskID, err)
	}
}

// whoMay is whether a rule's who lets someone (by uuid) move tasks to its
// status: anyone; the project's admins (manage, asked only then); or the
// people it names. The gate asks it of a move, and a board's read of each
// rule for its reader (MoveInto).
func whoMay(rule *model.StatusRule, byUUID string, manage func() bool) bool {
	switch rule.Who {
	case "admins":
		return manage()
	case "people":
		return slices.Contains(rule.People, byUUID)
	}
	return true
}

// The fields a rule may require, and how they read.
var requirable = map[string]string{
	"assignee":    "an assignee",
	"due":         "a due date",
	"tags":        "a tag",
	"description": "a description",
}

// approvedKey marks the move an approved request makes: its approval is
// done, the rest of the rule still holds.
type approvedKey struct{}

func withApproved(ctx context.Context, approvalID uuid.UUID) context.Context {
	return context.WithValue(ctx, approvedKey{}, approvalID)
}

func approvedMove(ctx context.Context) bool {
	_, ok := ctx.Value(approvedKey{}).(uuid.UUID)
	return ok
}

// Seams for tests.
var (
	rulesFor     = model.RulesFor
	request      = model.Request
	closeOpenFor = model.CloseOpenFor
	// The mover's standing in the task's project, read as they read the task.
	standingOf = func(ctx context.Context, by *dgraphStruct.DgraphUser, t *dgraphStruct.DgraphTask) (workAccess.Standing, error) {
		s, _, err := workAccess.OfTask(ctx, by.Uuid, by.Uid, t)
		return s, err
	}
	afterRequest = tellApprovers
)

// ruleFor is the rule a move into next meets (pick), read for it alone.
func ruleFor(ctx context.Context, projectID uuid.UUID, next taskStatusBusiness.Resolved) (*model.StatusRule, error) {
	rules, err := rulesFor(ctx, projectID, ruleKeys(next))
	if err != nil {
		return nil, err
	}
	return pick(rules, next), nil
}

// ruleKeys is the statuses whose rule a move into next may meet: a custom
// status's own first, then the built-in status it counts as.
func ruleKeys(next taskStatusBusiness.Resolved) []string {
	if next.CustomID != "" {
		return []string{next.CustomID, next.Category}
	}
	return []string{next.Category}
}

// pick is the rule of rules a move into next meets: the status's own, else
// the built-in status it counts as.
func pick(rules []*model.StatusRule, next taskStatusBusiness.Resolved) *model.StatusRule {
	for _, k := range ruleKeys(next) {
		for _, r := range rules {
			if r.Status == k {
				return r
			}
		}
	}
	return nil
}

// missing is what a task lacks of what a rule requires, in words.
func missing(t *dgraphStruct.DgraphTask, required []string) []string {
	var out []string
	for _, f := range required {
		has := true
		switch f {
		case "assignee":
			has = t.Assignee != nil && t.Assignee.Uuid != ""
		case "due":
			has = t.DueDate != nil && t.DueDate.Year() > 1970
		case "tags":
			has = t.Label != nil && len(helpers.Tags(*t.Label)) > 0
		case "description":
			has = t.Description != nil && strings.TrimSpace(helpers.RemoveHTMLTags(*t.Description)) != ""
		}
		if !has {
			out = append(out, requirable[f])
		}
	}
	return out
}

func and(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// cantCheck is the refusal of a move whose rules the gate couldn't read.
const cantCheck = "Couldn't check this status's rules. Try again in a moment."

// placed is the project and the id of the task a move was given, and
// whether both read.
func placed(task *dgraphStruct.DgraphTask) (projectID, taskID uuid.UUID, ok bool) {
	if task == nil || task.Project == nil {
		return uuid.Nil, uuid.Nil, false
	}
	projectID, perr := uuid.Parse(task.Project.Uuid)
	taskID, terr := uuid.Parse(task.Uuid)
	return projectID, taskID, perr == nil && terr == nil
}

// allowed is a rule's say on a move into its status (name) by who makes it
// and what the task has (whoMay, missing): nil, or why not in words. manage
// is asked only of a rule that's for admins.
func allowed(rule *model.StatusRule, task *dgraphStruct.DgraphTask, name string, by *dgraphStruct.DgraphUser, manage func() bool) error {
	if !whoMay(rule, by.Uuid, manage) {
		if rule.Who == "admins" {
			return &taskBusiness.StatusRefusal{Why: fmt.Sprintf("Only the project's admins move tasks to %s.", name)}
		}
		return &taskBusiness.StatusRefusal{Why: fmt.Sprintf("Only the people this project names move tasks to %s.", name)}
	}
	if lack := missing(task, rule.Required); len(lack) > 0 {
		return &taskBusiness.StatusRefusal{Why: fmt.Sprintf("Give it %s before moving it to %s.", and(lack), name)}
	}
	return nil
}

// firstStatuses is business/Task's FirstStatuses: a batch's new tasks, all in
// one project and made by one person (drafts, never empty), each judged as
// gate judges a move into its first status (pick, then allowed), from one
// read of the project's rules for every status the batch asks for. The
// maker's standing is the same for each of them, so it is read once, and
// only for a rule that's for admins. The first refusal refuses the batch;
// else a status that wants approval answers ErrWantsApproval.
func firstStatuses(ctx context.Context, drafts []*dgraphStruct.DgraphTask, next []taskStatusBusiness.Resolved, by *dgraphStruct.DgraphUser) error {
	projectID, _, ok := placed(drafts[0])
	if !ok || by == nil {
		helpers.LogErrorWithContext(ctx, "Approvals: %d new tasks came without their project, their ids or who made them, so they were held", len(drafts))
		return &taskBusiness.StatusRefusal{Why: cantCheck}
	}
	var keys []string
	for _, n := range next {
		keys = append(keys, ruleKeys(n)...)
	}
	slices.Sort(keys)
	rules, err := rulesFor(ctx, projectID, slices.Compact(keys))
	if err != nil {
		return &taskBusiness.StatusRefusal{Why: cantCheck}
	}
	met := make([]*model.StatusRule, len(drafts))
	admins := false
	for i := range drafts {
		met[i] = pick(rules, next[i])
		admins = admins || (met[i] != nil && met[i].Who == "admins")
	}
	manages := false
	if admins {
		s, err := standingOf(ctx, by, drafts[0])
		manages = err == nil && s.Can(workAccess.NeedManage)
	}
	wants := false
	for i, rule := range met {
		if rule == nil {
			continue
		}
		if err := allowed(rule, drafts[i], next[i].Display(), by, func() bool { return manages }); err != nil {
			return err
		}
		wants = wants || rule.WantsApproval()
	}
	if wants {
		return taskBusiness.ErrWantsApproval
	}
	return nil
}

// gate is business/Task's StatusGate. Whatever it can't check holds the move
// rather than let it past, a task with no project it can read included:
// every task is made in a project, so that is a read that left the project
// out, and letting the move through would skip all of the project's rules.
func gate(ctx context.Context, task *dgraphStruct.DgraphTask, prev, next taskStatusBusiness.Resolved, by *dgraphStruct.DgraphUser) error {
	projectID, taskID, ok := placed(task)
	if !ok || by == nil {
		helpers.LogErrorWithContext(ctx, "Approvals: a move to %s came without its task's project, its id or who made it, so it was held", next.Value())
		return &taskBusiness.StatusRefusal{Why: cantCheck}
	}
	rule, err := ruleFor(ctx, projectID, next)
	if err != nil {
		return &taskBusiness.StatusRefusal{Why: cantCheck}
	}
	if rule == nil {
		return nil
	}
	name := next.Display()
	manage := func() bool {
		s, err := standingOf(ctx, by, task)
		return err == nil && s.Can(workAccess.NeedManage)
	}
	if err := allowed(rule, task, name, by, manage); err != nil {
		return err
	}
	if !rule.WantsApproval() || approvedMove(ctx) {
		return nil
	}
	// A new task asks once it's made, as a move from its default status.
	if taskBusiness.FirstStatus(ctx) {
		return taskBusiness.ErrWantsApproval
	}
	var clients []string
	if rule.Client {
		clients, err = clientLinks(ctx, projectID, taskID)
		if errors.Is(err, ErrPrivateTask) {
			return &taskBusiness.StatusRefusal{Why: fmt.Sprintf("Moving it to %s needs the client's approval, and this task is private to some members.", name)}
		}
		if err != nil {
			return &taskBusiness.StatusRefusal{Why: "Couldn't ask for approval. Try again in a moment."}
		}
		if len(clients) == 0 {
			return &taskBusiness.StatusRefusal{Why: fmt.Sprintf("Moving it to %s needs the client's approval: share a client link that can comment first.", name)}
		}
	}
	// Only approvers who can see the task are asked, and the rule is never
	// lowered for those who can't answer: hiding a task from an approver must
	// not let it through on fewer yeses.
	approvers, cant, err := seeing(ctx, task, rule.Approvers)
	if err != nil {
		return &taskBusiness.StatusRefusal{Why: "Couldn't ask for approval. Try again in a moment."}
	}
	needed := 0
	if len(rule.Approvers) > 0 {
		needed = rule.ApprovalsNeeded
	}
	if len(approvers) < needed {
		return &taskBusiness.StatusRefusal{Why: short(name, needed, len(approvers), len(cant) > 0)}
	}
	requester, err := uuid.Parse(by.Uuid)
	if err != nil {
		return &taskBusiness.StatusRefusal{Why: fmt.Sprintf("Moving it to %s needs approval.", name)}
	}
	a, created, err := request(ctx, &model.Approval{
		ProjectID: projectID, TaskID: taskID, ToStatus: next.Value(), RequestedBy: requester,
		Approvers: approvers, Needed: needed, ClientGrants: clients,
	})
	if err != nil || a == nil {
		return &taskBusiness.StatusRefusal{Why: "Couldn't ask for approval. Try again in a moment."}
	}
	if !created {
		return &taskBusiness.StatusRefusal{Why: "It's waiting for approval already.", ApprovalID: a.ID.String()}
	}
	afterRequest(ctx, a, task, by, name)
	why := sentFor(name, needed, len(clients) > 0)
	if len(cant) > 0 {
		verb := "isn't"
		if len(cant) > 1 {
			verb = "aren't"
		}
		why += fmt.Sprintf(" %s can't see this task, so %s asked.", and(cant), verb)
	}
	return &taskBusiness.StatusRefusal{Why: why, ApprovalID: a.ID.String()}
}

// short says why a move waits when fewer of its rule's approvers can answer
// than it needs: some can't see the task (hidden), or the rest have left.
func short(name string, needed, can int, hidden bool) string {
	switch {
	case can == 0 && hidden:
		return fmt.Sprintf("Moving it to %s needs approval, and none of its approvers can see this task.", name)
	case can == 0:
		return fmt.Sprintf("Moving it to %s needs approval, and its approvers have left the workspace.", name)
	case hidden:
		return fmt.Sprintf("Moving it to %s needs %d approvals, and only %d of its approvers can see this task.", name, needed, can)
	case can == 1:
		return fmt.Sprintf("Moving it to %s needs %d approvals, and only 1 of its approvers is still in the workspace.", name, needed)
	}
	return fmt.Sprintf("Moving it to %s needs %d approvals, and only %d of its approvers are still in the workspace.", name, needed, can)
}

// sentFor says who a held move waits for.
func sentFor(name string, needed int, client bool) string {
	switch {
	case client && needed == 0:
		return fmt.Sprintf("Sent for the client's approval: it moves to %s once they approve.", name)
	case client:
		return fmt.Sprintf("Sent for approval: it moves to %s once %s and the client say yes.", name, approversWord(needed))
	case needed == 1:
		return fmt.Sprintf("Sent for approval: it moves to %s once it's approved.", name)
	}
	return fmt.Sprintf("Sent for approval: it moves to %s once %d approvers say yes.", name, needed)
}

func approversWord(n int) string {
	if n == 1 {
		return "an approver"
	}
	return fmt.Sprintf("%d approvers", n)
}
