package business

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Epic is the seventh built-in type (jira-01 J-H), seeded by its own
// migration the way 471 seeds the six, with an id that never changes.
func TestEpicIsTheSeventhBuiltInType(t *testing.T) {
	files, err := filepath.Glob("../../migrations/*_task_type_epic.up.sql")
	if err != nil || len(files) != 1 {
		t.Fatalf("want one task_type_epic migration, found %v (%v)", files, err)
	}
	sql, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile(`\('([0-9a-f-]{36})', 'epic', 'Epic', 'epic', 6\)`).FindStringSubmatch(string(sql))
	if row == nil || row[1] != TypeEpic.String() {
		t.Fatalf("the migration's epic row %v isn't TypeEpic %s", row, TypeEpic)
	}
	if !Builtin(TypeEpic) {
		t.Error("Epic isn't counted as built in")
	}
	found := false
	for _, i := range Icons {
		found = found || i == "epic"
	}
	if !found {
		t.Error("the epic glyph isn't one the app draws")
	}
}
