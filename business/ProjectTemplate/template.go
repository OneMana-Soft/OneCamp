// Package business (ProjectTemplate): templates to start a project from.
//
// A template is a project's shape: statuses of its own, and tasks, each with
// a status, a priority, tags, a description, subtasks, and dates counted in
// days from the day the project starts. Seven are built in (builtins.go). A
// project's admins save more, from the project as it is (snapshot.go) or from
// a template file, and everyone who can create a project sees them. Starting
// a project from one (apply.go) asks only the day it starts. Asana's and
// Linear's project templates, without the plan they sit behind.
package business

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Limits on a template.
const (
	MaxTasks           = 200 // tasks and subtasks together
	MaxSaved           = 200 // saved templates in a workspace
	MaxNameLength      = 60
	MaxAboutLength     = 280
	MaxTaskName        = 300
	MaxTaskDescription = 50000
	MaxDay             = 3650 // ten years from the start
	previewSize        = 4
)

// Template is a project template. ID is a built-in's slug or a saved one's uuid.
type Template struct {
	ID          string   `json:"id,omitempty"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Statuses    []Status `json:"statuses,omitempty"`
	Tasks       []Task   `json:"tasks"`
}

// Status is a status of the template's own: a name, the built-in status it
// counts as, and its colour, as a project's admins make one.
type Status = taskStatusBusiness.Input

// Task is one task a template makes. Status is a built-in key or the name of
// one of the template's statuses; none is To do. Days count from the day the
// project starts, which is day 0.
type Task struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"` // HTML, as the editor keeps it
	Status      string    `json:"status,omitempty"`
	Priority    string    `json:"priority,omitempty"`
	Tags        string    `json:"tags,omitempty"`
	StartDay    *int      `json:"start_day,omitempty"`
	DueDay      *int      `json:"due_day,omitempty"`
	Subtasks    []Subtask `json:"subtasks,omitempty"`
}

// Subtask is a step inside a task.
type Subtask struct {
	Name   string `json:"name"`
	DueDay *int   `json:"due_day,omitempty"`
}

// Size is how many tasks a template makes, subtasks included.
func (t Template) Size() int {
	n := len(t.Tasks)
	for _, task := range t.Tasks {
		n += len(task.Subtasks)
	}
	return n
}

// Preview is the names of its first few tasks, for the picker.
func (t Template) Preview() []string {
	out := []string{}
	for _, task := range t.Tasks {
		if len(out) == previewSize {
			break
		}
		out = append(out, task.Name)
	}
	return out
}

// TemplateError is a template that can't be used as it is; its text is
// written for the person who sent it.
type TemplateError struct{ msg string }

func (e *TemplateError) Error() string { return e.msg }

func fix(format string, a ...any) error { return &TemplateError{fmt.Sprintf(format, a...)} }

var (
	// ErrNotFound: no such template.
	ErrNotFound = errors.New("project template not found")
	// ErrNotYours: only a template's author or a workspace admin may delete it.
	ErrNotYours = errors.New("only the template's author or an admin can delete it")
)

// line is s with its spaces collapsed, or what to fix when it is empty or
// longer than max characters. what names the field in the message.
func line(s string, max int, what string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "", fix("Give %s a name.", what)
	}
	if utf8.RuneCountInString(s) > max {
		return "", fix("Keep %s's name under %d characters.", what, max)
	}
	return s, nil
}

// day checks a template day.
func day(d *int, task string) error {
	if d != nil && (*d < 0 || *d > MaxDay) {
		return fix("%q has a date %d days from the start; a template counts from 0 to %d.", task, *d, MaxDay)
	}
	return nil
}

func validPriority(p string) bool {
	if p == "" {
		return true
	}
	for _, v := range dgraphStruct.VALID_TASK_PRIORITIES {
		if v == p {
			return true
		}
	}
	return false
}

// Check is the template tidied as it is kept, or what to fix. Pure.
func Check(t Template) (Template, error) {
	var err error
	if t.Name, err = line(t.Name, MaxNameLength, "the template"); err != nil {
		return t, err
	}
	t.Description = strings.Join(strings.Fields(t.Description), " ")
	if utf8.RuneCountInString(t.Description) > MaxAboutLength {
		return t, fix("Keep the description under %d characters.", MaxAboutLength)
	}
	if len(t.Statuses) > taskStatusBusiness.MaxPerProject {
		return t, fix("A template has at most %d statuses of its own.", taskStatusBusiness.MaxPerProject)
	}
	// A task names a status of the template's own in any case; it is kept as
	// the status is spelled.
	own := map[string]string{}
	for i := range t.Statuses {
		if err := t.Statuses[i].Validate(); err != nil {
			return t, fix("The status %q: %s.", t.Statuses[i].Name, strings.TrimPrefix(err.Error(), taskStatusBusiness.ErrInvalid.Error()+": "))
		}
		key := strings.ToLower(t.Statuses[i].Name)
		if own[key] != "" {
			return t, fix("The template has two statuses named %q.", t.Statuses[i].Name)
		}
		own[key] = t.Statuses[i].Name
	}
	if len(t.Tasks) == 0 {
		return t, fix("A template needs at least one task.")
	}
	if n := t.Size(); n > MaxTasks {
		return t, fix("A template holds up to %d tasks and subtasks; this one has %d.", MaxTasks, n)
	}
	for i := range t.Tasks {
		task := &t.Tasks[i]
		if task.Name, err = line(task.Name, MaxTaskName, fmt.Sprintf("task %d", i+1)); err != nil {
			return t, err
		}
		if len(task.Description) > MaxTaskDescription {
			return t, fix("%q has a description longer than %d characters.", task.Name, MaxTaskDescription)
		}
		switch {
		case task.Status == "":
			task.Status = dgraphStruct.TASK_STATUS_TODO
		case taskStatusBusiness.IsBuiltIn(task.Status):
		case own[strings.ToLower(task.Status)] != "":
			task.Status = own[strings.ToLower(task.Status)]
		default:
			return t, fix("%q is in a status the template doesn't have: %q.", task.Name, task.Status)
		}
		if !validPriority(task.Priority) {
			return t, fix("%q has a priority OneCamp doesn't know: %q. Use low, medium or high.", task.Name, task.Priority)
		}
		task.Tags = helpers.NormaliseTags(task.Tags)
		if err := day(task.StartDay, task.Name); err != nil {
			return t, err
		}
		if err := day(task.DueDay, task.Name); err != nil {
			return t, err
		}
		if task.StartDay != nil && task.DueDay != nil && *task.StartDay > *task.DueDay {
			return t, fix("%q starts after it's due.", task.Name)
		}
		for j := range task.Subtasks {
			sub := &task.Subtasks[j]
			if sub.Name, err = line(sub.Name, MaxTaskName, fmt.Sprintf("subtask %d of %q", j+1, task.Name)); err != nil {
				return t, err
			}
			if err := day(sub.DueDay, sub.Name); err != nil {
				return t, err
			}
		}
	}
	return t, nil
}
