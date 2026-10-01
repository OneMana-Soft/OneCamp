package helpers

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The shared task read must enforce visibility, and background reads must say so.
//
// GET /task/info/{uuid} had its access check commented out, and the Dgraph query
// behind it has no membership filter, so any authenticated user could read any
// task in the workspace by uuid. The MCP reach tool, agent delegation and the
// agent work entity read through the same function and were equally open. The
// check now lives in business.GetDgraphTaskInfo so all of them inherit it.
//
// Pinned at the source because the predicate having tests does not prove the
// read CALLS it: deleting the call left every CanViewTask test passing.
func TestTaskReadEnforcesVisibility(t *testing.T) {
	src, err := os.ReadFile("../business/Task/taskBusiness.go")
	if err != nil {
		t.Fatalf("read taskBusiness.go: %v", err)
	}
	body := string(src)

	start := strings.Index(body, "func GetDgraphTaskInfo(")
	if start < 0 {
		t.Fatal("GetDgraphTaskInfo not found")
	}
	end := strings.Index(body[start:], "\nfunc ")
	if end < 0 {
		end = len(body) - start
	}
	fn := body[start : start+end]

	if !strings.Contains(fn, "CanViewTask(") {
		t.Error("GetDgraphTaskInfo does not call CanViewTask; every caller reads any task")
	}
	if !strings.Contains(fn, "ErrTaskNotVisible") {
		t.Error("GetDgraphTaskInfo does not refuse an invisible task")
	}
	if !strings.Contains(fn, "IsSystemRead(ctx)") {
		t.Error("GetDgraphTaskInfo has no system-read escape; background work will be refused")
	}
}

// A placeholder uid is not a person. Any read passing one must mark itself as
// the server's own, so every bypass of the access check is greppable.
func TestPlaceholderTaskReadsDeclareThemselvesSystem(t *testing.T) {
	// GetDgraphTaskInfo called with a literal uid ("0x1") or an empty one.
	placeholder := regexp.MustCompile(`GetDgraphTaskInfo\(([^)]*?),\s*("0x[0-9a-fA-F]+"|"")\s*\)`)
	var offenders []string

	for _, root := range []string{"../business", "../controllers"} {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			for _, m := range placeholder.FindAllStringSubmatch(string(src), -1) {
				if !strings.Contains(m[1], "WithSystemRead") {
					offenders = append(offenders, filepath.ToSlash(path)+"  "+truncate(m[0], 74))
				}
			}
			return nil
		})
	}

	if len(offenders) > 0 {
		t.Errorf("task reads passing a placeholder uid without helpers.WithSystemRead "+
			"(these silently bypass the access check):\n  %s", strings.Join(offenders, "\n  "))
	}
}
