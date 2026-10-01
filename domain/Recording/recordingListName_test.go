package domain

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// GetDgraphRecordingsList unmarshals the "recordingInfo" key and nothing else.
// A query fed to it whose block has another name returns an empty list with no
// error, which is how the recordings retention policy archived nothing for as
// long as it existed. The list blocks in this file and the key the parser
// reads must stay the same word.
func TestRecordingListBlocksAreNamedForTheParser(t *testing.T) {
	src, err := os.ReadFile("recordingDomian.go")
	if err != nil {
		t.Fatal(err)
	}
	model, err := os.ReadFile("../../models/dgraph/Recording/recordingModel.go")
	if err != nil {
		t.Fatal(err)
	}
	key := regexp.MustCompile(`RecordingInfo\s+\[\]\*dgraphStruct\.DgraphRecording\s+` + "`json:\"(\\w+)\"`").FindSubmatch(model)
	if key == nil {
		t.Fatal("cannot find the key GetDgraphRecordingsList parses; the model changed shape")
	}
	want := string(key[1])
	// Every list block: a name at the start of a line, then (func:, then a
	// body. Var blocks ("x as var(func:") and count blocks have no body the
	// parser reads and are not matched.
	lists := regexp.MustCompile(`(?m)^\s*(\w+)\(func:[^\n]*\{\s*$`).FindAllStringSubmatch(string(src), -1)
	if len(lists) < 3 {
		t.Fatalf("found %d list blocks; the regexp no longer matches the file", len(lists))
	}
	for _, b := range lists {
		if b[1] != want {
			t.Errorf("list block %q is not the key the parser reads (%q); it parses as an empty list", b[1], want)
		}
	}
	if strings.Contains(string(src), "recordings(func:") {
		t.Error("a block named recordings(func: is exactly the mistake this test exists for")
	}
}
