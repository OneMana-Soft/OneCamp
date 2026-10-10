package domain

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every DM query that lists participants must show bots.
//
// A DM with OneCamp AI once listed only the reader, because the participant
// filter dropped external users and bots are external by class. The sidebar
// then named the conversation after the reader and linked it to their own
// self-DM: two conversations under their own name, one holding a message from
// OneCamp AI.
func TestEveryDMParticipantListShowsBots(t *testing.T) {
	src, err := os.ReadFile("chatDomain.go")
	if err != nil {
		t.Fatal(err)
	}
	filters := regexp.MustCompile(`dm_participants @filter\(([^\n]*)\) \{`).FindAllStringSubmatch(string(src), -1)
	if len(filters) == 0 {
		t.Fatal("no participant filters found; this test would pass vacuously")
	}
	for _, f := range filters {
		if strings.Contains(f[1], "is_external") || !strings.Contains(f[1], "dmParticipantVisible") {
			t.Errorf("a DM participant filter hides bots, or repeats the rule instead of using dmParticipantVisible: %s", f[0])
		}
	}
	if !strings.Contains(dmParticipantVisible, "eq(is_bot, true)") {
		t.Fatal("the shared rule no longer admits bots")
	}
}
