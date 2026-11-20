package helpers

import (
	"fmt"
	"strings"
)

// UniqueName is name, numbered on from 2 ("Notes 2", "Notes 3") while one
// taken has it, ignoring case; taken holds the taken names in lower case, and
// the answer is added to it. A table's fields, its sheets, and the project
// and channel a table template makes are named by it.
func UniqueName(name string, taken map[string]bool) string {
	out := name
	for n := 2; taken[strings.ToLower(out)]; n++ {
		out = fmt.Sprintf("%s %d", name, n)
	}
	taken[strings.ToLower(out)] = true
	return out
}
