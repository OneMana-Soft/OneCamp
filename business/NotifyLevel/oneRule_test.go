package notifylevel

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The level rule has one copy, this package's (NOTIF M1). A query that picks
// whom to tell by a setting's value (notification_type = 'all', <> 'block',
// IN ('all', 'mention')) is a second copy: it decides in SQL what Lets
// decides, and the two drift (it dropped members a setting lets hear). A
// query reads people's settings and Lets or Hears says what each lets.
func TestNoQueryPicksWhomToTellByALevel(t *testing.T) {
	byLevel := regexp.MustCompile(`(?i)notification_type\s*(=|<>|!=|not\s+in|in)\s*\(?\s*'`)
	root := filepath.Join("..", "..")
	var found []string
	for _, dir := range []string{"business", "controllers", "domain", "models", "router", "helpers", "services"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(b), "\n") {
				if byLevel.MatchString(line) {
					rel, _ := filepath.Rel(root, path)
					found = append(found, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	for _, f := range found {
		t.Errorf("a query picks whom to tell by a level, a second copy of the rule: read the settings and ask notifylevel.Lets: %s", f)
	}
}
