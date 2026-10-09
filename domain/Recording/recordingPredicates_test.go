package domain

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// DeleteRecordingNodes deletes a recording by naming every predicate its node
// and its transcript lines hold. A predicate added to the schema's Recording
// or Transcript type and not to these lists would outlive every delete and
// purge, so this reads the schema and checks.
func TestDeletesNameEveryPredicateOfARecording(t *testing.T) {
	src, err := os.ReadFile("../../initializers/dgraphInit/connectDgraphDB.go")
	if err != nil {
		t.Fatal(err)
	}
	for typ, listed := range map[string][]string{"Recording": recordingPredicates, "Transcript": transcriptPredicates} {
		block := regexp.MustCompile(`(?s)type ` + typ + ` \{(.*?)\}`).FindSubmatch(src)
		if block == nil {
			t.Fatalf("cannot find type %s in the schema; it changed shape", typ)
		}
		var found int
		for _, line := range strings.Split(string(block[1]), "\n") {
			pred, _, _ := strings.Cut(strings.TrimSpace(line), ":")
			if pred = strings.TrimSpace(pred); pred == "" {
				continue
			}
			found++
			if !slices.Contains(listed, pred) {
				t.Errorf("%s.%s is in the schema but not deleted with a recording", typ, pred)
			}
		}
		if found < 4 {
			t.Fatalf("read %d predicates of %s; the parse no longer matches the schema", found, typ)
		}
		if !slices.Contains(listed, "dgraph.type") {
			t.Errorf("a %s's type isn't deleted with it", typ)
		}
	}
}
