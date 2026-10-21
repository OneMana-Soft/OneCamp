package helpers

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// idTextCompare is a uuid column cast to text and then compared: id::text =
// ANY($1), channel_uuid::text IN (...). Reading one out (SELECT id::text,
// RETURNING id::text) is fine.
var idTextCompare = regexp.MustCompile(`(?i)\b\w*(id|uuid)::text\s*(=|<>|!=|\bIN\s*\()`)

// No query compares a uuid column as text.
//
// WHY. id::text = ANY($1) casts every row's id before comparing, so the
// primary key's index can't be used and the delete or update scans the whole
// table: posts and chats (a forward's rollback), the archive's restore of
// posts, chats, tasks and attachments, and workspace memory's channel, project
// and author filters. Compare the column itself (id = ANY($1::uuid[])), with
// ids that are uuids.
func TestNoQueryComparesAUUIDColumnAsText(t *testing.T) {
	read := 0
	for _, root := range []string{"../business", "../models", "../domain", "../controllers", "../services"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			read++
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(b), "\n") {
				if m := idTextCompare.FindString(line); m != "" {
					t.Errorf("%s:%d: %q compares a uuid column as text, which no index serves; compare the column (= ANY($1::uuid[]))", path, i+1, m)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if read < 1000 {
		t.Fatalf("read %d files; the guard is no longer looking at the queries", read)
	}
}
