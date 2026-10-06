package controllers

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// "Add task" at the foot of a board column sends that column's status. The
// create handler used to overwrite it with Todo, so a task added under Done or
// In progress quietly landed in To do (v2.50.0 to v2.51.0). business.CreateTask
// resolves the status itself, an empty one to Todo; the handler must pass it on.
func TestCreateTaskKeepsTheStatusItWasGiven(t *testing.T) {
	src, err := os.ReadFile("Task/taskController.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "\nfunc CreateTask(")
	if start < 0 {
		t.Fatal("no CreateTask handler in Task/taskController.go")
	}
	end := strings.Index(body[start+1:], "\nfunc ")
	if end < 0 {
		end = len(body) - start - 1
	}
	if regexp.MustCompile(`\.Status\s*=[^=]`).MatchString(body[start : start+1+end]) {
		t.Error("CreateTask sets the task's status itself; leave it to business.CreateTask, which resolves what the caller sent")
	}
}
