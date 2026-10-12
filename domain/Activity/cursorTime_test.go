package domain

import (
	"os"
	"strings"
	"testing"
)

// The unified feed's cursor reached Dgraph cut to the second, so items older
// than the cursor but within its second were never listed.
func TestTheUnifiedFeedAsksDgraphToTheNanosecond(t *testing.T) {
	src, err := os.ReadFile("activityDomain.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "func GetUnifiedActivity(")
	if i < 0 {
		t.Fatal("no GetUnifiedActivity")
	}
	body := s[i:]
	if j := strings.Index(body[1:], "\nfunc "); j >= 0 {
		body = body[:j+1]
	}
	if !strings.Contains(body, `variables["$time"] = beforeTime.UTC().Format(time.RFC3339Nano)`) {
		t.Error("GetUnifiedActivity must send the cursor to Dgraph to the nanosecond")
	}
}
