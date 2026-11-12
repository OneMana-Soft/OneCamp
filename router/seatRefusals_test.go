package router

import (
	"os"
	"strings"
	"testing"
)

// The server links business/Seats, whose init has business/User tell the
// admins when the free plan's limit refuses someone and names the notice in
// the Inbox: without the import, nothing would.
func TestTheServerTellsAdminsOfSeatRefusals(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `_ "github.com/akashc777/OneCamp/business/Seats"`) {
		t.Fatal("router.go no longer links business/Seats")
	}
}
