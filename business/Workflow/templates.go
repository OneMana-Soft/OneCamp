package business

// Ready-made rules: the catalogue a project's Rules start from. Each is a
// rule as it is saved, in the project's words; one that needs a person or a
// status the project lacks is completed in the builder before it saves.

import workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"

// RuleTemplate is one ready-made rule.
type RuleTemplate struct {
	Key         string           `json:"key"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Hue         string           `json:"hue"` // Projects' camp hue: colour names an area (design v3.1); "agent" for AI
	TriggerType string           `json:"trigger_type"`
	Trigger     map[string]any   `json:"trigger_config,omitempty"`
	Conditions  Conditions       `json:"conditions"`
	Actions     []WorkflowAction `json:"actions"`
	ElseActions []WorkflowAction `json:"else_actions,omitempty"`
	// NeedsAI is a rule only the AI edition offers ("ask an agent").
	NeedsAI bool `json:"needs_ai,omitempty"`
}

func days(n int) *int { return &n }

// RuleTemplates is the catalogue.
var RuleTemplates = []RuleTemplate{
	{
		Key: "round-robin", Name: "Share new work out by turns", Hue: "moss",
		Description: "Each new task goes to the next person in the project.",
		TriggerType: workflowModel.TriggerTaskCreated,
		Conditions:  Conditions{Items: []Condition{{Field: "assignee", Op: "is_empty"}}},
		Actions:     []WorkflowAction{{Type: ActionAssign, Assign: AssignRoundRobin}},
	},
	{
		Key: "least-loaded", Name: "Give new work to whoever has least", Hue: "moss",
		Description: "A new task nobody has goes to the person with the fewest open tasks.",
		TriggerType: workflowModel.TriggerTaskCreated,
		Conditions:  Conditions{Items: []Condition{{Field: "assignee", Op: "is_empty"}}},
		Actions:     []WorkflowAction{{Type: ActionAssign, Assign: AssignLeastLoaded}},
	},
	{
		Key: "start-takes-it", Name: "Starting work makes it yours", Hue: "moss",
		Description: "Moving a task nobody has to In progress gives it to whoever moved it.",
		TriggerType: workflowModel.TriggerTaskStatusChanged, Trigger: map[string]any{"to_status": "inProgress"},
		Conditions: Conditions{Items: []Condition{{Field: "assignee", Op: "is_empty"}}},
		Actions:    []WorkflowAction{{Type: ActionAssign, Assign: AssignActor}},
	},
	{
		Key: "due-when-started", Name: "A due date when work starts", Hue: "moss",
		Description: "A task that starts without a due date is due in three days.",
		TriggerType: workflowModel.TriggerTaskStatusChanged, Trigger: map[string]any{"to_status": "inProgress"},
		Conditions: Conditions{Items: []Condition{{Field: "due", Op: "is_empty"}}},
		Actions:    []WorkflowAction{{Type: ActionSetDue, DueInDays: days(3)}},
	},
	{
		Key: "review-by-creator", Name: "Hand finished work to its creator", Hue: "moss",
		Description: "In review goes back to whoever made the task, with a note.",
		TriggerType: workflowModel.TriggerTaskStatusChanged, Trigger: map[string]any{"to_status": "inReview"},
		Actions: []WorkflowAction{
			{Type: ActionAssign, Assign: AssignCreator},
			{Type: ActionComment, Text: "Ready for your review: {by} moved it to {status}."},
		},
	},
	{
		Key: "tell-when-done", Name: "Tell the creator when it's done", Hue: "moss",
		Description: "Whoever asked for the work hears the moment it's finished.",
		TriggerType: workflowModel.TriggerTaskStatusChanged, Trigger: map[string]any{"to_status": "done"},
		Actions: []WorkflowAction{{Type: ActionNotify, Notify: NotifyCreator, Text: "{task} is done"}},
	},
	{
		Key: "bug-checklist", Name: "A checklist on every bug", Hue: "moss",
		Description: "A new task tagged bug gets three subtasks to finish it properly.",
		TriggerType: workflowModel.TriggerTaskCreated,
		Conditions:  Conditions{Items: []Condition{{Field: "tags", Op: "has", Value: "bug"}}},
		Actions: []WorkflowAction{
			{Type: ActionAddSubtask, TaskName: "Reproduce it"},
			{Type: ActionAddSubtask, TaskName: "Write a test that fails"},
			{Type: ActionAddSubtask, TaskName: "Fix and check"},
			{Type: ActionSetPriority, Priority: "high"},
		},
	},
	{
		Key: "high-needs-date", Name: "High priority needs a date", Hue: "moss",
		Description: "Raising a task to high gives it two days, if it had no due date.",
		TriggerType: workflowModel.TriggerTaskFieldChanged, Trigger: map[string]any{"fields": []string{"priority"}},
		Conditions: Conditions{Items: []Condition{{Field: "priority", Op: "is", Value: "high"}, {Field: "due", Op: "is_empty"}}},
		Actions:    []WorkflowAction{{Type: ActionSetDue, DueInDays: days(2)}},
	},
	{
		Key: "blocked-escalates", Name: "Blocked work gets attention", Hue: "moss",
		Description: "Tagging a task blocked raises it and tells whoever has it.",
		TriggerType: workflowModel.TriggerTaskFieldChanged, Trigger: map[string]any{"fields": []string{"tags"}},
		Conditions: Conditions{Items: []Condition{{Field: "tags", Op: "has", Value: "blocked"}}},
		Actions: []WorkflowAction{
			{Type: ActionSetPriority, Priority: "high"},
			{Type: ActionNotify, Notify: NotifyAssignee, Text: "{task} is blocked: {by} tagged it"},
		},
	},
	{
		Key: "done-needs-review", Name: "Done means reviewed", Hue: "moss",
		Description: "Work tagged needs review goes to In review instead; the rest tells its creator.",
		TriggerType: workflowModel.TriggerTaskStatusChanged, Trigger: map[string]any{"to_status": "done"},
		Conditions: Conditions{Items: []Condition{{Field: "tags", Op: "has", Value: "needs review"}}},
		Actions: []WorkflowAction{
			{Type: ActionSetStatus, Status: "inReview"},
			{Type: ActionComment, Text: "This needs a review before it's done."},
		},
		ElseActions: []WorkflowAction{{Type: ActionNotify, Notify: NotifyCreator, Text: "{task} is done"}},
	},
	// On time (R3).
	{
		Key: "monday-review", Name: "A review task every Monday", Hue: "moss",
		Description: "Every Monday morning, a task to review the week, due on Friday.",
		TriggerType: workflowModel.TriggerSchedule, Trigger: map[string]any{"repeat": "FREQ=WEEKLY;BYDAY=MO", "at": "09:00"},
		Actions: []WorkflowAction{
			{Type: ActionCreateTask, TaskName: "Review the week", Priority: "medium"},
			{Type: ActionSetDue, DueInDays: days(4)},
		},
	},
	{
		Key: "friday-overdue", Name: "Friday: chase what's overdue", Hue: "moss",
		Description: "Every Friday afternoon, whoever has an overdue task hears about it.",
		TriggerType: workflowModel.TriggerSchedule, Trigger: map[string]any{"repeat": "FREQ=WEEKLY;BYDAY=FR", "at": "16:00", "each_task": true},
		Conditions: Conditions{Items: []Condition{{Field: "due", Op: "overdue"}, {Field: "assignee", Op: "is_set"}}},
		Actions:    []WorkflowAction{{Type: ActionNotify, Notify: NotifyAssignee, Text: "{task} is overdue"}},
	},
	{
		Key: "due-tomorrow", Name: "Nudge the day before it's due", Hue: "moss",
		Description: "The morning before a task is due, whoever has it hears.",
		TriggerType: workflowModel.TriggerTaskDueSoon, Trigger: map[string]any{"days": 1, "at": "09:00"},
		Conditions: Conditions{Items: []Condition{{Field: "assignee", Op: "is_set"}}},
		Actions:    []WorkflowAction{{Type: ActionNotify, Notify: NotifyAssignee, Text: "{task} is due tomorrow"}},
	},
	{
		Key: "overdue-raises", Name: "Overdue work goes to the top", Hue: "moss",
		Description: "The morning after a task's due date passes, it becomes high priority.",
		TriggerType: workflowModel.TriggerTaskOverdue, Trigger: map[string]any{"at": "09:00"},
		Actions: []WorkflowAction{
			{Type: ActionSetPriority, Priority: "high"},
			{Type: ActionNotify, Notify: NotifyAssignee, Text: "{task} is overdue, so it's now high priority"},
		},
	},
	{
		Key: "agent-triages-bugs", Name: "An agent reads every new bug", Hue: "agent", NeedsAI: true,
		Description: "A new task tagged bug gets an AI teammate's first read: how bad, and what's missing.",
		TriggerType: workflowModel.TriggerTaskCreated,
		Conditions:  Conditions{Items: []Condition{{Field: "tags", Op: "has", Value: "bug"}}},
		Actions: []WorkflowAction{{Type: ActionAskAgent,
			Text: "Read this bug report. Say how severe it looks, what is missing to reproduce it, and what to check first."}},
	},
}

// Templates is the catalogue this build offers: without AI, none that asks
// an agent.
func Templates() []RuleTemplate {
	out := make([]RuleTemplate, 0, len(RuleTemplates))
	ai := asker() != nil
	for _, t := range RuleTemplates {
		if t.NeedsAI && !ai {
			continue
		}
		out = append(out, t)
	}
	return out
}
