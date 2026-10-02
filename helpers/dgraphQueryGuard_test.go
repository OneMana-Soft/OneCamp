package helpers

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A Dgraph query that names the same field twice in one block is refused
// whole ("while converting to subgraph: <field>"). One duplicated
// user_full_name made the meeting-recordings list fail for every caller for
// weeks, and nothing but the logs said so. This reads every query literal in
// the source and checks that no block repeats a field.
func TestNoDgraphQueryRepeatsAFieldInOneBlock(t *testing.T) {
	literal := regexp.MustCompile("(?s)`([^`]*)`")
	plainField := regexp.MustCompile(`^[A-Za-z_~][\w.~]*$`)
	var hits []string
	scanned := 0
	for _, root := range []string{"../domain", "../models", "../business", "../controllers", "../services"} {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, _ := os.ReadFile(path)
			for _, m := range literal.FindAllSubmatchIndex(src, -1) {
				q := string(src[m[2]:m[3]])
				if !strings.Contains(q, "(func:") {
					continue
				}
				scanned++
				firstLine := strings.Count(string(src[:m[2]]), "\n") + 1
				stack := []map[string]bool{{}}
				for i, line := range strings.Split(q, "\n") {
					s := strings.TrimSpace(line)
					if s == "" || strings.HasPrefix(s, "#") {
						continue
					}
					// The field this line names, if it names one: "user_name",
					// "recording_dm {", "ch_recs as ch_recording @filter(...)". Query
					// roots ("name(func: ...)") and var blocks are not fields.
					head := strings.TrimSpace(strings.SplitN(s, "{", 2)[0])
					head = strings.TrimSpace(strings.SplitN(head, "@", 2)[0])
					if parts := strings.Fields(head); len(parts) == 3 && parts[1] == "as" {
						head = parts[2]
					}
					if plainField.MatchString(head) && head != "var" && !strings.HasPrefix(head, "query") {
						if stack[len(stack)-1][head] {
							hits = append(hits, path+":"+strconv.Itoa(firstLine+i)+": "+head)
						}
						stack[len(stack)-1][head] = true
					}
					for n := strings.Count(s, "{"); n > 0; n-- {
						stack = append(stack, map[string]bool{})
					}
					for n := strings.Count(s, "}"); n > 0 && len(stack) > 1; n-- {
						stack = stack[:len(stack)-1]
					}
				}
			}
			return nil
		})
	}
	if scanned < 50 {
		t.Fatalf("found only %d Dgraph queries; the scan has stopped seeing them", scanned)
	}
	if len(hits) > 0 {
		t.Errorf("a field repeated in one block makes Dgraph refuse the whole query:\n  %s", strings.Join(hits, "\n  "))
	}
}
