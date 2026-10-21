package controllers

import (
	"os"
	"strings"
	"testing"
)

// PR1: the task lists' controllers hold no graph query text; a list's filters
// are built in one place (domain/Task.TaskListFilter). Zero baseline.
func TestTaskListControllersHoldNoQueryText(t *testing.T) {
	for _, file := range []string{"projectController.go", "../User/userController.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, frag := range []string{"regexp(", "uid_in(", "anyofterms(", "ge(task_", "lt(task_"} {
			if strings.Contains(string(src), frag) {
				t.Errorf("%s builds graph query text (%s): build it in domain/Task.TaskListFilter", file, frag)
			}
		}
	}
}
