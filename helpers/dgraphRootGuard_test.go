package helpers

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// hasRootThenNarrow is a query that starts from every node with a predicate
// and only then keeps the ones it named (uid(...)) or the ones linked to one
// node (uid_in(edge, $var)): has(ch_uuid) @filter(uid(...)), has(ch_uuid)
// @filter(uid_in(~user_channels, $user_id)).
var hasRootThenNarrow = regexp.MustCompile(`func:\s*has\(\w+\)(,\s*first:[^)]*)?\)\s*@filter\(\s*\(*\s*(uid\(|uid_in\(~?\w+,\s*\$)`)

// A Dgraph query that knows which nodes it wants starts from them.
//
// WHY. Six reads started from has(...) — every channel, every project, every
// person in the workspace — and then filtered down to the ids they were given,
// or to what one person is linked to: the channel list on every load, the
// archived list, the admin's projects, the member flags. Each read the whole
// type to return a handful. Start from uid(...), or walk the edge from the one
// node (uid($user_id) { user_channels }, ~project_admins), and filter what
// that reaches.
func TestDgraphQueriesStartFromTheNodesTheyWant(t *testing.T) {
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
				if m := hasRootThenNarrow.FindString(line); m != "" {
					t.Errorf("%s:%d: %q starts from every node and then narrows; start from uid(...) or walk the edge from the node", path, i+1, m)
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
