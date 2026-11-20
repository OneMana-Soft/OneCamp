package Domain

import (
	"regexp"
	"strings"
	"testing"
)

// Unlinking every task writes only the tasks holdsGitHub names, so it must
// name a task holding any field the unlink clears (V-1029): a field missing
// from it is left behind on a task holding only that field.
func TestUnlinkingEveryTaskReachesATaskHoldingAnyFieldTheUnlinkClears(t *testing.T) {
	cleared := regexp.MustCompile(`(github_[a-z_]+) = `).FindAllStringSubmatch(unlinkFromGitHub, -1)
	if len(cleared) < 10 {
		t.Fatalf("unlinkFromGitHub clears %d fields, want every GitHub field (10)", len(cleared))
	}
	for _, m := range cleared {
		if !regexp.MustCompile(`\b` + m[1] + `\b`).MatchString(holdsGitHub) {
			t.Errorf("holdsGitHub doesn't name %s, which unlinkFromGitHub clears", m[1])
		}
	}
	if strings.Contains(holdsGitHub, "updated_at") {
		t.Error("holdsGitHub names updated_at, which every task has")
	}
}
