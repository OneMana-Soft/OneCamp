package helpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A move of data tells nobody (NOTIF-H6). What an import, a sync, the demo's
// seed or an import's re-check writes into a project was there already, so
// the task tellers (business/Task: a new task's push, notice and email, a
// comment's, an assignment's, a status's) stay quiet for it: each such write
// passes taskBusiness.QuietMove(ctx). A plain or DataMove ctx tells, as a
// person's write does; the packages that move data name each write that
// tells on purpose below, with why. A new write there tells nobody until it
// says so.

var dataMovePackages = []string{"GitHub", "Import", "ProjectTemplate", "DemoSeed", "SlackImport"}

var dataMoveTellers = []string{"taskBusiness.CreateTask(", "taskBusiness.CreateTasks(", "taskBusiness.CreateTaskComment(", "taskBusiness.UpdateTaskAssigneeByTaskUUID(", "taskBusiness.UpdateTaskStatusByTaskUUID("}

// toldOnPurpose is each write in those packages that tells, by its file and
// the start of its call, and why.
var toldOnPurpose = map[string]string{
	"business/ProjectTemplate/apply.go|taskBusiness.CreateTasks(taskBusiness.DataMove(ctx),":                                            "a template's apply is a person's act: its tasks are told in ONE batch push",
	"business/GitHub/githubBusiness.go|taskBusiness.CreateTaskComment(ctx, taskUUID, dgraphTaskInfo, userInfo,":                         "a comment written on GitHub now is told as one written here is",
	"business/GitHub/githubBusiness.go|taskBusiness.UpdateTaskAssigneeByTaskUUID(taskBusiness.DataMove(ctx), taskUUID, userDgraphInfo,": "whom an issue is assigned to on GitHub, a OneCamp person, is told once (the assignment's dedup key)",
	"business/GitHub/githubBusiness.go|taskBusiness.UpdateTaskAssigneeByTaskUUID(ctx, taskUUID, nil,":                                   "taking the assignee off tells nobody: there is no one to tell",
	"business/Import/recheck.go|taskBusiness.UpdateTaskAssigneeByTaskUUID(ctx, t.TaskID, nil,":                                          "taking the assignee off tells nobody: there is no one to tell",
	"business/GitHub/githubBusiness.go|taskBusiness.UpdateTaskStatusByTaskUUID(ctx, taskUUID, to.Value(),":                              "a link's rule moving a task on a pull request's or an issue's event on GitHub now is told as a move made here is",
	"business/GitHub/githubBusiness.go|taskBusiness.UpdateTaskStatusByTaskUUID(ctx, taskUUID, defaultStatus,":                           "an issue closed or reopened on GitHub now is told as a move made here is",
}

func TestEveryWriteOfADataMoveIsQuiet(t *testing.T) {
	used := map[string]bool{}
	for _, pkg := range dataMovePackages {
		dir := filepath.Join("..", "business", pkg)
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Errorf("%s: no Go files; the guard looks at nothing there", dir)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			file := strings.TrimPrefix(filepath.ToSlash(path), "../")
			for i, line := range strings.Split(string(src), "\n") {
				code := strings.TrimSpace(line)
				if strings.HasPrefix(code, "//") {
					continue
				}
				for _, teller := range dataMoveTellers {
					at := strings.Index(code, teller)
					if at < 0 {
						continue
					}
					call := code[at:]
					if strings.HasPrefix(call, teller+"taskBusiness.QuietMove(ctx)") {
						continue
					}
					why := ""
					for key, reason := range toldOnPurpose {
						f, start, _ := strings.Cut(key, "|")
						if f == file && strings.HasPrefix(call, start) {
							why, used[key] = reason, true
						}
					}
					if why == "" {
						t.Errorf("%s:%d: %s\n\ta move of data tells nobody: pass taskBusiness.QuietMove(ctx), or name the write in toldOnPurpose with why it tells",
							file, i+1, code)
					}
				}
			}
		}
	}
	for key := range toldOnPurpose {
		if !used[key] {
			t.Errorf("toldOnPurpose names %q, which no longer matches a write: take it out", key)
		}
	}
}
